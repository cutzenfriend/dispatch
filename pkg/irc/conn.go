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
	ErrBadProtocol = errors.New("This server does not speak IRC")
)

func (c *Client) Connect() {
	c.connChange(false, nil)
	go c.run()
}

func (c *Client) Reconnect() {
	close(c.reconnect)
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

	addr := net.JoinHostPort(c.Config.Host, c.Config.Port)
	if c.Config.TLS {
		conn, err := tls.DialWithDialer(c.dialer, "tcp", addr, c.Config.TLSConfig)
		if err != nil {
			return err
		}

		c.conn = conn
	} else {
		conn, err := c.dialer.Dial("tcp", addr)
		if err != nil {
			return err
		}

		c.conn = conn
	}

	c.connected = true
	c.connChange(true, nil)
	c.scan = bufio.NewScanner(c.conn)
	c.scan.Buffer(c.recvBuf, cap(c.recvBuf))

	c.register()

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
				c.connChange(false, nil)
				c.Reconnect()
				return
			}
		}

		b := bytes.Trim(c.scan.Bytes(), " ")
		if len(b) == 0 {
			continue
		}

		msg := ParseMessage(string(b))
		if msg == nil {
			close(c.quit)
			c.connChange(false, ErrBadProtocol)
			return
		}

		switch msg.Command {
		case PING:
			go c.write("PONG :" + msg.LastParam())

		case JOIN:
			if c.Is(msg.Sender) {
				c.addChannel(msg.Params[0])
			}

		case NICK:
			if c.Is(msg.Sender) {
				c.setNick(msg.LastParam())
			}

		case PRIVMSG:
			if ctcp := msg.ToCTCP(); ctcp != nil {
				c.handleCTCP(ctcp, msg)
			}

		case CAP:
			c.handleCAP(msg)

		case RPL_WELCOME:
			c.setNick(msg.Params[0])
			c.setRegistered(true)
			c.flushChannels()

			c.backoff.Reset()
			c.sendRecv.Add(1)
			go c.send()

		case RPL_ISUPPORT:
			c.Features.Parse(msg.Params)

		case ERR_NICKNAMEINUSE, ERR_NICKCOLLISION, ERR_UNAVAILRESOURCE:
			if c.Config.HandleNickInUse != nil {
				go c.writeNick(c.Config.HandleNickInUse(msg.Params[1]))
			}

		case ERROR:
			c.Messages <- msg
			c.connChange(false, nil)
			time.Sleep(5 * time.Second)
			close(c.quit)
			return
		}

		c.handleSASL(msg)

		c.Messages <- msg
	}
}
