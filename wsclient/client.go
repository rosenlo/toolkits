package wsclient

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rosenlo/toolkits/log"
	"github.com/rosenlo/toolkits/promutil"
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
	// Timestamp allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Timestamp allowed to read the next pong message from the peer.
	pongWait = 10 * time.Second

	readTimeout = 5 * time.Minute

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512
)

type Client struct {
	URL         *url.URL
	Name        string
	conn        *websocket.Conn
	done        chan struct{}
	connectedAt time.Time
	msgCount    int64
}

type ResponseMessage struct {
	Id          string
	Host        string
	Type        int
	Body        []byte
	ReceiveTime time.Time
}

func New(addr string, header http.Header) (*Client, error) {
	u, err := url.Parse(addr)
	if err != nil {
		return nil, fmt.Errorf("url Parse: %s", err)
	}

	c, resp, err := websocket.DefaultDialer.Dial(u.String(), header)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			return nil, fmt.Errorf("dial: %s(%s); resp code: %d; resp headers: %v; resp body: %s", err, u.String(), resp.StatusCode, resp.Header, string(body))
		}
		return nil, fmt.Errorf("dial: %s(%s)", err, u.String())
	}

	client := &Client{
		URL:         u,
		conn:        c,
		done:        make(chan struct{}),
		connectedAt: time.Now(),
	}

	return client, nil
}

func (c *Client) GetRemoteAddr() string {
	return c.conn.RemoteAddr().String()
}

func (c *Client) Stop() {
	if err := c.conn.Close(); err != nil {
		log.Warnf("error close websocket connection: %s", err)
	}
}

func (c *Client) PingPong(ctx context.Context) {
	c.conn.SetPongHandler(func(string) error {
		log.Debugf("receive pong message from %s", c.URL.String())
		if err := c.conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
			log.Warnf("error set read deadline: %s", err)
		}
		return nil
	})

	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-c.done:
			return
		case <-ticker.C:
			err := c.conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(writeWait))
			if err != nil {
				log.Errorf("write ping message: %s", err)
				return
			}
		}
	}
}

func (c *Client) SendCloseMessage() {
	log.Debugf("send close message to %s", c.URL.String())
	// Cleanly close the connection by sending a close message and then
	// waiting (with timeout) for the server to close the connection.
	err := c.conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(writeWait))
	if err != nil {
		log.Errorf("error send close message to %s: %s", c.URL.String(), err)
	}
	c.Stop()
}

func (c *Client) Receive(ctx context.Context, messageCh chan *ResponseMessage) {
	go c.PingPong(ctx)

	go func() {
		defer close(c.done)
		for {
			if err := c.conn.SetReadDeadline(time.Now().Add(readTimeout)); err != nil {
				log.Warnf("error set read deadline: %s", err)
			}
			messageType, message, err := c.conn.ReadMessage()
			if err != nil {
				log.Errorf("read: %s %s, name: %s, remoteAddr: %s, messageType: %d, messagesReceived: %d, connDuration: %s",
					err, c.URL.String(), c.Name, c.conn.RemoteAddr(), messageType, c.msgCount, time.Since(c.connectedAt))
				return
			}
			c.msgCount++
			messageCh <- &ResponseMessage{Host: c.URL.Host, Type: messageType, Body: message, ReceiveTime: time.Now()}

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
		case <-c.done:
			c.SendCloseMessage()
			return
		case <-ctx.Done():
			c.SendCloseMessage()
			select {
			case <-c.done:
			case <-time.After(time.Second):
			}
			return
		}
	}
}

func (c *Client) WriteMessage(msg []byte) error {
	return c.conn.WriteMessage(websocket.TextMessage, msg)
}

func (c *Client) WriteJSON(payload any) error {
	return c.conn.WriteJSON(payload)
}
