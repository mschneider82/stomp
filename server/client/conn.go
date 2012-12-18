package client

import (
	"fmt"
	"github.com/jjeffery/stomp/message"
	"io"
	"log"
	"net"
	"strconv"
	"time"
)

// Maximum number of pending frames allowed to a client.
// before a disconnect occurs. If the client cannot keep
// up with the server, we do not want the server to backlog
// pending frames indefinitely.
const maxPendingWrites = 16

// Maximum number of pending frames allowed before the read
// go routine starts blocking.
const maxPendingReads = 16

// Represents a connection with the STOMP client.
type Conn struct {
	config         Config
	rw             net.Conn                              // Network connection to client
	writer         *message.Writer                       // Writes STOMP frames directly to the network connection
	requestChannel chan Request                          // For sending requests to upper layer
	subChannel     chan *Subscription                    // Receives subscription messages for client
	writeChannel   chan *message.Frame                  // Receives unacknowledged (topic) messages for client
	readChannel    chan *message.Frame                   // Receives frames from the client
	stateFunc      func(c *Conn, f *message.Frame) error // State processing function
	readTimeout    time.Duration                         // Heart beat read timeout
	writeTimeout   time.Duration                         // Heart beat write timeout
	version        message.StompVersion                  // Negotiated STOMP protocol version
	closed         bool                                  // Is the connection closed
	txStore        *txStore                              // Stores transactions in progress
	lastMsgId uint64 // last message-id value
	subList *SubscriptionList // List of subscriptions requiring acknowledgement
	subs map[string]*Subscription // All subscriptions, keyed by id
}

// Creates a new client connection. The config parameter contains
// process-wide configuration parameters relevant to a client connection.
// The rw parameter is a network connection object for communicating with
// the client. All client requests are sent via the ch channel to the
// upper layer.
func NewConn(config Config, rw net.Conn, ch chan Request) *Conn {
	c := &Conn{
		config:         config,
		rw:             rw,
		requestChannel: ch,
		subChannel:     make(chan *Subscription, maxPendingWrites),
		writeChannel:   make(chan *message.Frame, maxPendingWrites),
		readChannel:    make(chan *message.Frame, maxPendingReads),
		txStore:        &txStore{},
		subList: NewSubscriptionList(),
	}
	go c.readLoop()
	go c.processLoop()
	return c
}

// Write a frame to the connection without requiring
// any acknowledgement.
func (c *Conn) Send(f *message.Frame) {
	// Place the frame on the write channel. If the
	// write channel is full, the caller will block.
	c.writeChannel <- f
}

// Send and ERROR message to the client. The client
// connection will disconnect as soon as the ERROR
// message has been transmitted. The message header 
// will be based on the contents of the err parameter.
func (c *Conn) SendError(err error) {
	f := new(message.Frame)
	f.Command = message.ERROR
	f.Headers.Append(message.Message, err.Error())
	c.Send(f) // will close after successful send
}

// Send an ERROR frame to the client and immediately. The error
// message is derived from err. If f is non-nil, it is the frame
// whose contents have caused the error. Include the receipt-id 
// header if the frame contains a receipt header.
func (c *Conn) sendErrorImmediately(err error, f *message.Frame) {
	errorFrame := message.NewFrame(message.ERROR,
		message.Message, err.Error())

	// Include a receipt-id header if the frame that prompted the error had
	// a receipt header (as suggested by the STOMP protocol spec).
	if f != nil {
		if receipt, ok := f.Contains(message.Receipt); ok {
			errorFrame.Append(message.ReceiptId, receipt)
		}
	}

	// send the frame to the client, ignore any error condition
	// because we are about to close the connection anyway
	_ = c.sendImmediately(errorFrame)
}

// Sends a STOMP frame to the client immediately, does not push onto the
// write channel to be processed in turn.
func (c *Conn) sendImmediately(f *message.Frame) error {
	return c.writer.Write(f)
}

// Go routine for reading bytes from a client and assembling into
// STOMP frames. Also handles heart-beat read timeout. All read
// frames are pushed onto the read channel to be processed by the
// processLoop go-routine. This keeps all processing of frames for
// this connection on the one go-routine and avoids race conditions.
func (c *Conn) readLoop() {
	reader := message.NewReader(c.rw)
	expectingConnect := true
	readTimeout := time.Duration(0)
	for {
		if readTimeout == time.Duration(0) {
			// infinite timeout
			c.rw.SetReadDeadline(time.Time{})
		} else {
			c.rw.SetReadDeadline(time.Now().Add(readTimeout))
		}
		f, err := reader.Read()
		if err != nil {
			if err == io.EOF {
				log.Println("connection closed:", c.rw.RemoteAddr())
			} else {
				log.Println("read failed:", err, ":", c.rw.RemoteAddr())
			}

			// Close the read channel so that the processing loop will 
			// know to terminate, if it has not already done so. This is
			// the only channel that we close, because it is the only one
			// we know who is writing to.
			close(c.readChannel)
			return
		}

		if f == nil {
			// if the frame is nil, then it is a heartbeat
			continue
		}

		// If we are expecting a CONNECT or STOMP command, extract
		// the heart-beat header and work out the read timeout.
		// Note that the processing loop will duplicate this to
		// some extent, but letting this go-routine work out its own
		// read timeout means no synchronization is necessary.
		if expectingConnect {
			// Expecting a CONNECT or STOMP command, get the heart-beat
			cx, _, err := f.HeartBeat()
			
			// Ignore the error condition and treat as no read timeout.
			// The processing loop will handle the error again and 
			// process correctly.
			if err == nil {
				// Minimum value as per server config. If the client
				// has requested shorter periods than this value, the
				// server will insist on the longer time period.
				min := asMilliseconds(c.config.HeartBeat(), message.MaxHeartBeat)

				// apply a minimum heartbeat 
				if cx > 0 && cx < min {
					cx = min
				}

				readTimeout = time.Duration(cx) * time.Millisecond
				
				expectingConnect = false
			}
		}

		// Add the frame to the read channel. Note that this will block
		// if we are reading from the client quicker than the server
		// can process frames.
		c.readChannel <- f
	}
}

// Go routine that processes all read frames and all write frames.
// Having all processing in one go routine helps eliminate any race conditions.
func (c *Conn) processLoop() {
	defer c.cleanupConn()

	c.writer = message.NewWriter(c.rw)
	c.stateFunc = connecting
	for {
		var timerChannel <-chan time.Time
		var timer *time.Timer

		if c.writeTimeout > 0 {
			timer = time.NewTimer(c.writeTimeout)
			timerChannel = timer.C
		}

		select {
		case f, ok := <-c.writeChannel:
			if !ok {
				// write channel has been closed, so
				// exit go-routine (after cleaning up)
				return
			}
			
			// have a frame to the client with 
			// no acknowledgement required (topic)
			
			// stop the heart-beat timer
			if timer != nil {
				timer.Stop()
				timer = nil
			}
			
			c.allocateMessageId(f, nil)

			// write the frame to the client
			err := c.writer.Write(f)
			if err != nil {
				// if there is an error writing to
				// the client, there is not much
				// point trying to send an ERROR frame,
				// so just exit go-routine (after cleaning up)
				return
			}

			// if the frame just sent to the client is an error
			// frame, we disconnect
			if f.Command == message.ERROR {
				// sent an ERROR frame, so disconnect
				return
			}

		case f, ok := <-c.readChannel:
			if !ok {
				// read channel has been closed, so
				// exit go-routine (after cleaning up)
				return
			}
			
			// Just received a frame from the client.
			// Validate the frame, checking for mandatory
			// headers and prohibited headers.
			err := f.Validate()
			if err != nil {
				c.sendErrorImmediately(err, f)
				return
			}

			// Pass to the appropriate function for handling
			// according to the current state of the connection.
			err = c.stateFunc(c, f)
			if err != nil {
				c.sendErrorImmediately(err, f)
				return
			}
			
		case sub, ok := <-c.subChannel:
			if !ok {
				// subscription channel has been closed,
				// so exit go-routine (after cleaning up)
				return
			}
			
			// have a frame to the client which requires
			// acknowledgement to the upper layer
			
			// stop the heart-beat timer
			if timer != nil {
				timer.Stop()
				timer = nil
			}

			// allocate a message-id, note that the
			// subscription id has already been set
			c.allocateMessageId(sub.frame, sub)

			// write the frame to the client
			err := c.writer.Write(sub.frame)
			if err != nil {
				// if there is an error writing to
				// the client, there is not much
				// point trying to send an ERROR frame,
				// so just exit go-routine (after cleaning up)
				return
			}
			
			if sub.ack == message.AckAuto {
				// subscription does not require acknowledgement,
				// so send the subscription back the upper layer
				// straight away
				c.requestChannel <- Request{Op: SubscribeOp, Sub: sub}
			} else {
				// subscription requires acknowledgement
				c.subList.Add(sub)
			}

		case _ = <-timerChannel:
			// write a heart-beat
			err := c.writer.Write(nil)
			if err != nil {
				return
			}
		}
	}
}

// Called when the connection is closing, and takes care of
// unsubscribing all subscriptions with the upper layer, and
// re-queueing all unacknowledged messages to the upper layer.
func (c *Conn) cleanupConn() {
	// clean up any pending transactions
	c.txStore.Init()
	
	c.discardWriteChannelFrames()

	// Unsubscribe every subscription known to the upper layer.
	// This should be done before cleaning up the subscription
	// channel. If we requeued messages before doing this,
	// we might end up getting them back again.
	for _, sub := range c.subs {
		// Note that we only really need to send a request if the
		// subscription does not have a frame, but for simplicity
		// all subscriptions are unsubscribed from the upper layer.
		c.requestChannel <- Request{Op: UnsubscribeOp, Sub: sub}
	}
	
	// Clear out the map of subscriptions
	c.subs = nil
	
	// Every subscription requiring acknowledgement has a frame
	// that needs to be requeued in the upper layer
	for sub:= c.subList.Get(); sub != nil; sub = c.subList.Get() {
		c.requestChannel <- Request{Op: RequeueOp, Frame: sub.frame}
	}
	
	// empty the subscription and write queue
	c.discardWriteChannelFrames()
	c.cleanupSubChannel()
	
	// Tell the upper layer we are now disconnected
	c.requestChannel <- Request{Op: DisconnectedOp, Conn: c}
	
	// empty the subscription and write queue one more time
	c.discardWriteChannelFrames()
	c.cleanupSubChannel()
	
	// Should not hurt to call this if it is already closed?
	c.rw.Close()
}

// Discard anything on the write channel. These frames
// do not get acknowledged, and are either topic MESSAGE 
// frames or ERROR frames.
func (c *Conn) discardWriteChannelFrames() {
	for finished := false; !finished; {
		select {
		case _, ok := <- c.writeChannel:
			if !ok {
				finished = true
			}
			
		default:
			finished = true
		}
	}
}

func (c *Conn) cleanupSubChannel() {
	// Read the subscription channel until it is empty.
	// Each frame should be requeued to the upper layer.
	for finished:= false; !finished; {
		select {
			case sub, ok := <- c.subChannel:
				if !ok {
					finished = true
				}
				c.requestChannel <- Request{Op: RequeueOp, Frame: sub.frame}
				
			default:
				finished = true
		}
	}
}

// Send a frame to the client, allocating necessary headers prior.
func (c *Conn) allocateMessageId(f *message.Frame, sub *Subscription) {
	if f.Command == message.MESSAGE {
		// allocate the value of message-id for this frame
		c.lastMsgId++
		messageId := strconv.FormatUint(c.lastMsgId, 10)
		f.Set(message.MessageId, messageId)
		
		// if there is any requirement by the client to acknowledge, set
		// the ack header as per STOMP 1.2
		if sub.ack == message.AckAuto {
			f.Remove(message.Ack)
		} else {
			f.Set(message.Ack, messageId)
		}
	}
}

func (c *Conn) handleConnect(f *message.Frame) error {
	var err error

	if _, ok := f.Contains(message.Receipt); ok {
		// CONNNECT and STOMP frames are not allowed to have
		// a receipt header.
		return receiptInConnect
	}

	// if either of these fields are absent, pass nil to the
	// authenticator function.
	login, _ := f.Contains(message.Login)
	passcode, _ := f.Contains(message.Passcode)
	if !c.config.Authenticate(login, passcode) {
		// sleep to slow down a rogue client a little bit
		time.Sleep(time.Second)
		return authenticationFailed
	}

	c.version, err = f.AcceptVersion()
	if err != nil {
		return err
	}

	cx, cy, err := f.HeartBeat()
	if err != nil {
		return err
	}

	// Minimum value as per server config. If the client
	// has requested shorter periods than this value, the
	// server will insist on the longer time period.
	min := asMilliseconds(c.config.HeartBeat(), message.MaxHeartBeat)

	// apply a minimum heartbeat 
	if cx > 0 && cx < min {
		cx = min
	}
	if cy > 0 && cy < min {
		cy = min
	}

	c.readTimeout = time.Duration(cx) * time.Millisecond
	c.writeTimeout = time.Duration(cy) * time.Millisecond

	// Note that the heart-beat header is included even if the
	// client is V1.0 and did not send a header. This should not
	// break V1.0 clients.
	response := message.NewFrame(message.CONNECTED,
		message.Version, string(c.version),
		message.Server, "stompd/x.y.z", // TODO: get version
		message.HeartBeat, fmt.Sprintf("%d,%d", cy, cx))

	c.Send(response)
	c.stateFunc = connected

	// tell the upper layer we are connected
	//	c.requestChannel <- request{op: connectOp, conn: c}

	return nil
}

func connecting(c *Conn, f *message.Frame) error {
	switch f.Command {
	case message.CONNECT, message.STOMP:
		return c.handleConnect(f)
	}
	return notConnected
}

// Sends a RECEIPT frame to the client if the frame f contains
// a receipt header. If the frame does contain a receipt header,
// it will be removed from the frame.
func (c *Conn) sendReceiptImmediately(f *message.Frame) error {
	if receipt, ok := f.Contains(message.Receipt); ok {
		// Remove the receipt header from the frame. This is handy
		// for transactions, because the frame has its receipt 
		// header removed prior to entering the transaction store.
		// When the frame is processed upon transaction commit, it
		// will not have a receipt header anymore.
		f.Remove(message.Receipt)
		return c.sendImmediately(message.NewFrame(message.RECEIPT, message.ReceiptId, receipt))
	}
	return nil
}

func (c *Conn) handleDisconnect(f *message.Frame) error {
	// As soon as we receive a DISCONNECT frame from a client, we do
	// not want to send any more frames to that client, with the exception
	// of a RECEIPT frame if the client has requested one.
	// Ignore the error condition if we cannot send a RECEIPT frame,
	// as the connection is about to close anyway.
	_ = c.sendReceiptImmediately(f)
	return nil
}

func (c *Conn) handleBegin(f *message.Frame) error {
	// the frame should already have been validated for the
	// transaction header, but we check again here.
	if transaction, ok := f.Contains(message.Transaction); ok {
		// Send a receipt and remove the header
		err := c.sendReceiptImmediately(f)
		if err != nil {
			return err
		}

		return c.txStore.Begin(transaction)
	}
	return missingHeader
}

func (c *Conn) handleCommit(f *message.Frame) error {
	// the frame should already have been validated for the
	// transaction header, but we check again here.
	if transaction, ok := f.Contains(message.Transaction); ok {
		// Send a receipt and remove the header
		err := c.sendReceiptImmediately(f)
		if err != nil {
			return err
		}
		return c.txStore.Commit(transaction, func(f *message.Frame) error {
			// Call the state function (again) for each frame in the
			// transaction. This time each frame is stripped of its transaction
			// header (and its receipt header as well, if it had one).
			return c.stateFunc(c, f)
		})
	}
	return missingHeader
}

func (c *Conn) handleAbort(f *message.Frame) error {
	// the frame should already have been validated for the
	// transaction header, but we check again here.
	if transaction, ok := f.Contains(message.Transaction); ok {
		// Send a receipt and remove the header
		err := c.sendReceiptImmediately(f)
		if err != nil {
			return err
		}
		return c.txStore.Abort(transaction)
	}
	return missingHeader
}

// Handle a SEND frame received from the client. Note that
// this method is called after a SEND message is received,
// but also after a transaction commit.
func (c *Conn) handleSend(f *message.Frame) error {
	// Send a receipt and remove the header
	err := c.sendReceiptImmediately(f)
	if err != nil {
		return err
	}

	if tx, ok := f.Contains(message.Transaction); ok {
		// the transaction header is removed from the frame
		err = c.txStore.Add(tx, f)
		if err != nil {
			return err
		}
	} else {
		// not in a transaction, send to be processed
		c.requestChannel <- Request{Op: EnqueueOp, Frame: f}
	}

	return nil
}

// Send the frame to the request channel. Remove receipt header
// and send a RECEIPT frame to the client if necessary.
func (c *Conn) sendFrameRequest(f *message.Frame) error {
	// Send a receipt and remove the header
	err := c.sendReceiptImmediately(f)
	if err != nil {
		return err
	}

	// Handled by next level
	c.requestChannel <- Request{Op: EnqueueOp, Frame: f}
	return nil
}

func (c *Conn) handleSubscribe(f *message.Frame) error {
	return c.sendFrameRequest(f)
}

func (c *Conn) handleUnsubscribe(f *message.Frame) error {
	return c.sendFrameRequest(f)
}

func (c *Conn) handleAck(f *message.Frame) error {
	return c.sendFrameRequest(f)
}

func (c *Conn) handleNack(f *message.Frame) error {
	return c.sendFrameRequest(f)
}

func connected(c *Conn, f *message.Frame) error {
	switch f.Command {
	case message.CONNECT, message.STOMP:
		return unexpectedCommand
	case message.DISCONNECT:
		return c.handleDisconnect(f)
	case message.BEGIN:
		return c.handleBegin(f)
	case message.ABORT:
		return c.handleAbort(f)
	case message.COMMIT:
		return c.handleCommit(f)
	case message.SEND:
		return c.handleSend(f)
	case message.SUBSCRIBE:
		return c.handleSubscribe(f)
	case message.UNSUBSCRIBE:
		return c.handleUnsubscribe(f)
	case message.ACK:
		return c.handleAck(f)
	case message.NACK:
		return c.handleNack(f)
	case message.MESSAGE, message.RECEIPT, message.ERROR:
		// should only be sent by the server, should not come from the client
		return unexpectedCommand
	default:
		return unknownCommand
	}
	panic("not reached")
}
