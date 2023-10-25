package websocketclient

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"time"

	"github.com/rosenlo/toolkits/promutil"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
)

var messageChanSize *prometheus.GaugeVec

const (
	MetricMessageChan = "wsp_message_channel_size"
)

var MessageChanLabel = []string{"url"}

func init() {
	messageChanSize, _ = promutil.NewGaugeVec(MetricMessageChan, "", MessageChanLabel)
}

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 1 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

type Client struct {
	URL  *url.URL
	conn *websocket.Conn
}

type Message struct {
	Type        int
	Body        []byte
	ReceiveTime time.Time
}

func New(addr string) (*Client, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("url Parse: %s", err)
	}

	c, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("dial: %s", err)
	}

	return &Client{
		URL:  u,
		conn: c,
	}, nil
}

func (c *Client) Stop() {
	c.conn.Close()
}

func (c *Client) Recv(ctx context.Context, messageCh chan<- Message) {
	defer c.Stop()

	//ticker := time.NewTicker(pingPeriod)
	//defer ticker.Stop()

	// c.SetReadDeadline(time.Now().Add(pongWait))
	// c.SetPongHandler(func(string) error { c.SetReadDeadline(time.Now().Add(pongWait)); return nil })

	done := make(chan struct{})

	go func() {
		defer close(done)
		defer close(messageCh)
		for {
			messageType, message, err := c.conn.ReadMessage()
			if err != nil {
				log.Printf("read: %s %s", err, c.URL.String())
				return
			}
			messageCh <- Message{Type: messageType, Body: message, ReceiveTime: time.Now()}

			select {
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
	}()

	ticker := time.NewTicker(time.Second * 5)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			messageChanSize.WithLabelValues(c.URL.String()).Set(float64(len(messageCh)))
		case <-done:
			return
		case <-ctx.Done():
			log.Printf("send close message to %s", c.URL.String())
			// Cleanly close the connection by sending a close message and then
			// waiting (with timeout) for the server to close the connection.
			err := c.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			if err != nil {
				log.Println("write close:", err)
				return
			}
			select {
			case <-done:
			case <-time.After(time.Second):
			}
			return
		}
	}
}
