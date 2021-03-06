package irc

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"time"
)

var (
	DefaultDialer = &net.Dialer{Timeout: 10 * time.Second}

	ErrBadProtocol = errors.New("This server does not speak IRC")
)

type Dialer interface {
	Dial(network, address string) (net.Conn, error)
}

func (c *Client) Connect() {
	go c.run()
}

func (c *Client) Reconnect() {
	c.tryConnect()
}

func (c *Client) Write(data string) {
	c.out <- data + "\r\n"
}

func (c *Client) Writef(format string, a ...interface{}) {
	c.out <- fmt.Sprintf(format+"\r\n", a...)
}

func (c *Client) write(data string) {
	c.conn.Write([]byte(data + "\r\n"))
}

func (c *Client) writef(format string, a ...interface{}) {
	fmt.Fprintf(c.conn, format+"\r\n", a...)
}

func (c *Client) run() {
	c.tryConnect()

	for {
		select {
		case <-c.quit:
			c.setRegistered(false)
			if c.Connected() {
				c.disconnect()
			}

			c.sendRecv.Wait()
			close(c.Messages)
			return

		case <-c.reconnect:
			c.setRegistered(false)
			if c.Connected() {
				c.disconnect()
			}

			c.sendRecv.Wait()
			c.reconnect = make(chan struct{})
			c.state.reset()
			c.initSASL()

			time.Sleep(c.backoff.Duration())
			c.tryConnect()
		}
	}
}

type ConnectionState struct {
	Connected bool
	Error     error
}

func (c *Client) connChange(connected bool, err error) {
	c.ConnectionChanged <- ConnectionState{
		Connected: connected,
		Error:     err,
	}
}

func (c *Client) disconnect() {
	c.lock.Lock()
	c.connected = false
	c.lock.Unlock()

	c.conn.Close()
}

func (c *Client) tryConnect() {
	for {
		select {
		case <-c.quit:
			return

		default:
		}

		err := c.connect()
		if err != nil {
			c.connChange(false, err)
			if _, ok := err.(x509.UnknownAuthorityError); ok {
				return
			}
		} else {
			return
		}

		time.Sleep(c.backoff.Duration())
	}
}

func (c *Client) connect() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	conn, err := c.dialer.Dial("tcp", net.JoinHostPort(c.Config.Host, c.Config.Port))
	if err != nil {
		return err
	}

	if c.Config.TLS {
		c.Config.TLSConfig.ServerName = c.Config.Host

		tlsConn := tls.Client(conn, c.Config.TLSConfig)
		err = tlsConn.Handshake()
		if err != nil {
			return err
		}
		conn = tlsConn
	}

	c.conn = conn
	c.connected = true
	c.connChange(true, nil)
	c.scan = bufio.NewScanner(c.conn)
	c.scan.Buffer(c.recvBuf, cap(c.recvBuf))

	go c.register()

	c.sendRecv.Add(1)
	go c.recv()

	return nil
}

func (c *Client) send() {
	defer c.sendRecv.Done()

	for {
		select {
		case <-c.quit:
			return

		case <-c.reconnect:
			return

		case msg := <-c.out:
			_, err := c.conn.Write([]byte(msg))
			if err != nil {
				return
			}
		}
	}
}

func (c *Client) recv() {
	defer c.sendRecv.Done()

	for {
		if !c.scan.Scan() {
			select {
			case <-c.quit:
				return

			default:
				c.connChange(false, c.scan.Err())
				close(c.reconnect)
				return
			}
		}

		b := bytes.Trim(c.scan.Bytes(), " ")
		if len(b) == 0 {
			continue
		}

		msg := ParseMessage(string(b))
		if msg == nil {
			c.connChange(false, ErrBadProtocol)
			close(c.quit)
			return
		}

		c.handleMessage(msg)

		c.Messages <- msg
	}
}
