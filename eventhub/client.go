package eventhub

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"gopkg.in/satori/go.uuid.v1"
	"pack.ag/amqp"
)

// Dial connects to the named amqp broker and returns a eventhub client.
func Dial(hostname string) (*Client, error) {
	conn, err := amqp.Dial("amqps://" + hostname)
	if err != nil {
		return nil, err
	}
	sess, err := conn.NewSession()
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &Client{
		conn: conn,
		sess: sess,
		done: make(chan struct{}),
	}, nil
}

// Client is eventhub client.
type Client struct {
	mu   sync.Mutex
	conn *amqp.Client
	sess *amqp.Session
	done chan struct{}
}

func (c *Client) Sess() *amqp.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

func (c *Client) SubscribePartitions(ctx context.Context, name, group string, f func(*amqp.Message)) error {
	return SubscribePartitions(ctx, c.sess, name, group, f)
}

func SubscribePartitions(ctx context.Context, sess *amqp.Session, name, group string, f func(*amqp.Message)) error {
	ids, err := getPartitionIDs(ctx, sess, name)
	if err != nil {
		return err
	}

	// stop all goroutines at return.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	msgc := make(chan *amqp.Message, len(ids))
	errc := make(chan error, len(ids))
	for _, id := range ids {
		recv, err := sess.NewReceiver(
			amqp.LinkSourceAddress(fmt.Sprintf("/%s/ConsumerGroups/%s/Partitions/%s", name, group, id)),

			// TODO: make it configurable
			amqp.LinkSelectorFilter(fmt.Sprintf("amqp.annotation.x-opt-enqueuedtimeutc > '%d'",
				time.Now().UnixNano()/int64(time.Millisecond)),
			),
		)
		if err != nil {
			return err
		}

		go func(r *amqp.Receiver) {
			defer recv.Close()
			for {
				msg, err := r.Receive(ctx)
				if err != nil {
					errc <- err
					return
				}
				msg.Accept()
				msgc <- msg
			}
		}(recv)
	}

	for {
		select {
		case msg := <-msgc:
			go f(msg)
		case err := <-errc:
			return err
		}
	}
}

// PutTokenContinuously writes token first time in blocking mode and returns
// maintaining token updates in the background until stopCh is closed.
func (c *Client) PutTokenContinuously(
	ctx context.Context,
	audience string,
	token string,
	stopCh chan struct{},
) error {
	if err := c.PutToken(ctx, audience, token); err != nil {
		return err
	}
	go func() {
		for {
			select {
			case <-time.After(time.Hour): // TODO: bigger update interval
				if err := c.PutToken(ctx, audience, token); err != nil {
					log.Printf("put token error: %s", err)
					return
				}
			case <-stopCh:
				return
			}
		}
	}()
	return nil
}

func (c *Client) PutToken(ctx context.Context, audience, token string) error {
	send, err := c.sess.NewSender(
		amqp.LinkTargetAddress("$cbs"),
	)
	if err != nil {
		return err
	}
	defer send.Close()

	recv, err := c.sess.NewReceiver(amqp.LinkSourceAddress("$cbs"))
	if err != nil {
		return err
	}
	defer recv.Close()

	if err = send.Send(ctx, &amqp.Message{
		Value: token,
		Properties: &amqp.MessageProperties{
			MessageID: uuid.NewV4().String(),
			To:        "$cbs",
			ReplyTo:   "cbs",
		},
		ApplicationProperties: map[string]interface{}{
			"operation": "put-token",
			"type":      "servicebus.windows.net:sastoken",
			"name":      audience,
		},
	}); err != nil {
		return err
	}

	msg, err := recv.Receive(ctx)
	if err != nil {
		return err
	}
	if err = CheckMessageResponse(msg); err != nil {
		return err
	}
	msg.Accept()
	return nil
}

// Close closes amqp session and connection.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return nil
	default:
		close(c.done)
	}
	if err := c.sess.Close(); err != nil {
		return err
	}
	return c.conn.Close()
}

// getPartitionIDs returns partition ids for the named eventhub.
func getPartitionIDs(ctx context.Context, sess *amqp.Session, name string) ([]string, error) {
	replyTo := uuid.NewV4().String()
	recv, err := sess.NewReceiver(
		amqp.LinkSourceAddress("$management"),
		amqp.LinkTargetAddress(replyTo),
	)
	if err != nil {
		return nil, err
	}
	defer recv.Close()

	send, err := sess.NewSender(
		amqp.LinkTargetAddress("$management"),
		amqp.LinkSourceAddress(replyTo),
	)
	if err != nil {
		return nil, err
	}
	defer send.Close()

	msgID := uuid.NewV4().String()
	if err := send.Send(ctx, &amqp.Message{
		Properties: &amqp.MessageProperties{
			MessageID: msgID,
			ReplyTo:   replyTo,
		},
		ApplicationProperties: map[string]interface{}{
			"operation": "READ",
			"name":      name,
			"type":      "com.microsoft:eventhub",
		},
	}); err != nil {
		return nil, err
	}

	msg, err := recv.Receive(ctx)
	if err != nil {
		return nil, err
	}
	if err = CheckMessageResponse(msg); err != nil {
		return nil, err
	}
	if msg.Properties.CorrelationID != msgID {
		return nil, errors.New("message-id mismatch")
	}
	msg.Accept()

	val, ok := msg.Value.(map[string]interface{})
	if !ok {
		return nil, errors.New("unable to typecast value")
	}
	ids, ok := val["partition_ids"].([]string)
	if !ok {
		return nil, errors.New("unable to typecast partition_ids")
	}
	return ids, nil
}

// CheckMessageResponse checks for 200 response code otherwise returns an error.
func CheckMessageResponse(msg *amqp.Message) error {
	rc, ok := msg.ApplicationProperties["status-code"].(int32)
	if !ok {
		return errors.New("unable to typecast status-code")
	}
	if rc == 200 {
		return nil
	}
	rd, _ := msg.ApplicationProperties["status-description"]
	return fmt.Errorf("code = %d, description = %q", rc, rd)
}