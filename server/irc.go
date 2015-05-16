package server

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	PING    = "PING"
	NICK    = "NICK"
	JOIN    = "JOIN"
	PART    = "PART"
	MODE    = "MODE"
	PRIVMSG = "PRIVMSG"
	NOTICE  = "NOTICE"
	TOPIC   = "TOPIC"
	QUIT    = "QUIT"

	RPL_WELCOME       = "001"
	RPL_YOURHOST      = "002"
	RPL_CREATED       = "003"
	RPL_LUSERCLIENT   = "251"
	RPL_LUSEROP       = "252"
	RPL_LUSERUNKNOWN  = "253"
	RPL_LUSERCHANNELS = "254"
	RPL_LUSERME       = "255"

	RPL_AWAY = "301"

	RPL_WHOISUSER     = "311"
	RPL_WHOISSERVER   = "312"
	RPL_WHOISOPERATOR = "313"
	RPL_WHOISIDLE     = "317"
	RPL_ENDOFWHOIS    = "318"
	RPL_WHOISCHANNELS = "319"

	RPL_TOPIC = "332"

	RPL_NAMREPLY   = "353"
	RPL_ENDOFNAMES = "366"

	RPL_MOTD      = "372"
	RPL_MOTDSTART = "375"
	RPL_ENDOFMOTD = "376"
)

type Message struct {
	Prefix   string
	Command  string
	Params   []string
	Trailing string
}

type IRC struct {
	conn     net.Conn
	reader   *bufio.Reader
	out      chan string
	ready    sync.WaitGroup
	once     sync.Once
	nick     string
	nickLock sync.Mutex

	Messages  chan *Message
	Server    string
	Host      string
	TLS       bool
	TLSConfig *tls.Config
	Password  string
	Username  string
	Realname  string
}

func NewIRC(nick, username string) *IRC {
	return &IRC{
		nick:     nick,
		Username: username,
		Realname: nick,
		Messages: make(chan *Message, 32),
		out:      make(chan string, 32),
	}
}

func (i *IRC) Connect(address string) error {
	if idx := strings.Index(address, ":"); idx < 0 {
		i.Host = address

		if i.TLS {
			address += ":6697"
		} else {
			address += ":6667"
		}
	} else {
		i.Host = address[:idx]
	}
	i.Server = address

	dialer := &net.Dialer{Timeout: 30 * time.Second}

	if i.TLS {
		if i.TLSConfig == nil {
			i.TLSConfig = &tls.Config{InsecureSkipVerify: true}
		}

		if conn, err := tls.DialWithDialer(dialer, "tcp", address, i.TLSConfig); err != nil {
			return err
		} else {
			i.conn = conn
		}
	} else {
		if conn, err := dialer.Dial("tcp", address); err != nil {
			return err
		} else {
			i.conn = conn
		}
	}

	i.reader = bufio.NewReader(i.conn)

	if i.Password != "" {
		i.Pass(i.Password)
	}
	i.Nick(i.nick)
	i.User(i.Username, i.Realname)

	i.ready.Add(1)
	go i.send()
	go i.recv()

	return nil
}

func (i *IRC) Pass(password string) {
	i.write("PASS " + password)
}

func (i *IRC) Nick(nick string) {
	i.write("NICK " + nick)

	i.nickLock.Lock()
	i.nick = nick
	i.nickLock.Unlock()
}

func (i *IRC) User(username, realname string) {
	i.writef("USER %s 0 * :%s", username, realname)
}

func (i *IRC) Oper(name, password string) {
	i.Write("OPER " + name + " " + password)
}

func (i *IRC) Mode(target, modes, params string) {
	i.Write(strings.TrimRight("MODE "+target+" "+modes+" "+params, " "))
}

func (i *IRC) Quit() {
	go func() {
		i.ready.Wait()
		i.write("QUIT")
		i.conn.Close()
	}()
}

func (i *IRC) Join(channels ...string) {
	i.Write("JOIN " + strings.Join(channels, ","))
}

func (i *IRC) Part(channels ...string) {
	i.Write("PART " + strings.Join(channels, ","))
}

func (i *IRC) Topic(channel string) {
	i.Write("TOPIC " + channel)
}

func (i *IRC) Invite(nick, channel string) {
	i.Write("INVITE " + nick + " " + channel)
}

func (i *IRC) Kick(channel string, users ...string) {
	i.Write("KICK " + channel + " " + strings.Join(users, ","))
}

func (i *IRC) Privmsg(target, msg string) {
	i.Writef("PRIVMSG %s :%s", target, msg)
}

func (i *IRC) Notice(target, msg string) {
	i.Writef("NOTICE %s :%s", target, msg)
}

func (i *IRC) Whois(nick string) {
	i.Write("WHOIS " + nick)
}

func (i *IRC) Away(message string) {
	i.Write("AWAY :" + message)
}

func (i *IRC) GetNick() string {
	i.nickLock.Lock()
	defer i.nickLock.Unlock()

	return i.nick
}

func (i *IRC) Write(data string) {
	i.out <- data + "\r\n"
}

func (i *IRC) Writef(format string, a ...interface{}) {
	i.out <- fmt.Sprintf(format+"\r\n", a...)
}

func (i *IRC) write(data string) {
	i.conn.Write([]byte(data + "\r\n"))
}

func (i *IRC) writef(format string, a ...interface{}) {
	fmt.Fprintf(i.conn, format+"\r\n", a...)
}

func (i *IRC) send() {
	i.ready.Wait()
	for {
		_, err := i.conn.Write([]byte(<-i.out))
		if err != nil {
			return
		}
	}
}

func (i *IRC) recv() {
	defer i.close()
	for {
		line, err := i.reader.ReadString('\n')
		if err != nil {
			log.Println("IRC connection to", i.Server, "died")
			return
		}

		msg := parseMessage(line)
		msg.Prefix = parseUser(msg.Prefix)

		i.Messages <- msg

		switch msg.Command {
		case PING:
			go i.write("PONG :" + msg.Trailing)

		case RPL_WELCOME:
			i.once.Do(i.ready.Done)
		}
	}
}

func (i *IRC) close() {
	i.conn.Close()
	i.once.Do(i.ready.Done)
	close(i.out)
	close(i.Messages)
}

func parseMessage(line string) *Message {
	line = strings.Trim(line, "\r\n")
	msg := Message{}
	cmdStart := 0
	cmdEnd := len(line)

	if strings.HasPrefix(line, ":") {
		cmdStart = strings.Index(line, " ") + 1
		msg.Prefix = line[1 : cmdStart-1]
	}

	if i := strings.Index(line, " :"); i > 0 {
		cmdEnd = i
		msg.Trailing = line[i+2:]
	}

	cmd := strings.Split(line[cmdStart:cmdEnd], " ")
	msg.Command = cmd[0]
	if len(cmd) > 1 {
		msg.Params = cmd[1:]
	}

	if msg.Trailing != "" {
		msg.Params = append(msg.Params, msg.Trailing)
	}

	return &msg
}

func parseUser(user string) string {
	if i := strings.Index(user, "!"); i > 0 {
		return user[:i]
	}
	return user
}
