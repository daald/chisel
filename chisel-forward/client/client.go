package chiselclient

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hashicorp/yamux"
	"github.com/jpillora/backoff"
	"github.com/jpillora/chisel"
	"golang.org/x/net/websocket"
)

type Client struct {
	*chisel.Logger
	config    *chisel.Config
	encconfig string
	proxies   []*Proxy
	session   *yamux.Session
	running   bool
	runningc  chan error
}

func NewClient(auth, server string, remotes ...string) (*Client, error) {

	//apply default scheme
	if !strings.HasPrefix(server, "http") {
		server = "http://" + server
	}

	u, err := url.Parse(server)
	if err != nil {
		return nil, err
	}

	//apply default port
	if !regexp.MustCompile(`:\d+$`).MatchString(u.Host) {
		if u.Scheme == "https" || u.Scheme == "wss" {
			u.Host = u.Host + ":443"
		} else {
			u.Host = u.Host + ":80"
		}
	}

	//swap to websockets scheme
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)

	config := &chisel.Config{
		Version: chisel.ProtocolVersion,
		Auth:    auth,
		Server:  u.String(),
	}

	for _, s := range remotes {
		r, err := chisel.DecodeRemote(s)
		if err != nil {
			return nil, fmt.Errorf("Failed to decode remote '%s': %s", s, err)
		}
		config.Remotes = append(config.Remotes, r)
	}

	encconfig, err := chisel.EncodeConfig(config)
	if err != nil {
		return nil, fmt.Errorf("Failed to encode config: %s", err)
	}

	return &Client{
		Logger:    chisel.NewLogger("client"),
		config:    config,
		encconfig: encconfig,
		running:   true,
		runningc:  make(chan error, 1),
	}, nil
}

//Start then Wait
func (c *Client) Run() error {
	go c.start()
	return c.Wait()
}

//Starts the client
func (c *Client) Start() {
	go c.start()
}

func (c *Client) start() {
	c.Infof("Connecting to %s\n", c.config.Server)

	//proxies all use this function
	openStream := func() (net.Conn, error) {
		if c.session == nil || c.session.IsClosed() {
			return nil, c.Errorf("no session available")
		}
		stream, err := c.session.Open()
		if err != nil {
			return nil, err
		}
		return stream, nil
	}

	//prepare proxies
	for id, r := range c.config.Remotes {
		proxy := NewProxy(c, id, r, openStream)
		go proxy.start()
		c.proxies = append(c.proxies, proxy)
	}

	var connerr error
	b := &backoff.Backoff{Max: 5 * time.Minute}

	//connection loop!
	for {
		if !c.running {
			break
		}
		if connerr != nil {
			connerr = nil
			d := b.Duration()
			c.Infof("Retrying in %s...\n", d)
			time.Sleep(d)
		}

		ws, err := websocket.Dial(c.config.Server, c.encconfig, "http://localhost/")
		if err != nil {
			connerr = err
			continue
		}

		buff := make([]byte, 0xff)
		n, _ := ws.Read(buff)
		if msg := string(buff[:n]); msg != "handshake-success" {
			//no point in retrying
			c.runningc <- errors.New(msg)
			ws.Close()
			break
		}

		// Setup client side of yamux
		c.session, err = yamux.Client(ws, nil)
		if err != nil {
			connerr = err
			continue
		}
		b.Reset()

		//signal is connected
		connected := make(chan bool)
		c.Infof("Connected\n")

		//poll websocket state
		go func() {
			for {
				if c.session.IsClosed() {
					connerr = c.Errorf("disconnected")
					c.Infof("Disconnected\n")
					close(connected)
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}()
		//block!
		<-connected
	}
	close(c.runningc)
}

//Wait blocks while the client is running
func (c *Client) Wait() error {
	return <-c.runningc
}

//Close manual stops the client
func (c *Client) Close() error {
	c.running = false
	if c.session == nil {
		return nil
	}
	return c.session.Close()
}