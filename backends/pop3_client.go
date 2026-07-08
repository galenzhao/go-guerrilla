package backends

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

type pop3Client struct {
	conn   net.Conn
	reader *textproto.Reader
	writer *textproto.Writer
}

func dialPOP3(host string, port int, useTLS bool) (*pop3Client, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var conn net.Conn
	var err error
	if useTLS {
		conn, err = tls.Dial("tcp", addr, &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		})
	} else {
		conn, err = net.DialTimeout("tcp", addr, 30*time.Second)
	}
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(5 * time.Minute))
	reader := textproto.NewReader(bufio.NewReader(conn))
	writer := textproto.NewWriter(bufio.NewWriter(conn))
	c := &pop3Client{conn: conn, reader: reader, writer: writer}
	if _, err := c.readPositive(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *pop3Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	_, _ = c.cmd("QUIT")
	return c.conn.Close()
}

func (c *pop3Client) cmd(format string, args ...interface{}) (string, error) {
	line := fmt.Sprintf(format, args...)
	if err := c.writer.PrintfLine("%s", line); err != nil {
		return "", err
	}
	return c.readResponse()
}

func (c *pop3Client) readResponse() (string, error) {
	line, err := c.reader.ReadLine()
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(line, "+OK") {
		if len(line) > 3 {
			return strings.TrimSpace(line[3:]), nil
		}
		return "", nil
	}
	if strings.HasPrefix(line, "-ERR") {
		return "", fmt.Errorf("pop3 error: %s", line)
	}
	return "", fmt.Errorf("pop3 unexpected response: %q", line)
}

func (c *pop3Client) readPositive() (string, error) {
	line, err := c.reader.ReadLine()
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(line, "+OK") {
		return "", fmt.Errorf("pop3 error: %s", line)
	}
	if len(line) > 3 {
		return strings.TrimSpace(line[3:]), nil
	}
	return "", nil
}

func (c *pop3Client) readMultiline() ([]string, error) {
	var lines []string
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return nil, err
		}
		if line == "." {
			return lines, nil
		}
		if strings.HasPrefix(line, "..") {
			line = line[1:]
		}
		lines = append(lines, line)
	}
}

func (c *pop3Client) Auth(user, password string) error {
	if _, err := c.cmd("USER %s", user); err != nil {
		return err
	}
	_, err := c.cmd("PASS %s", password)
	return err
}

type pop3UIDL struct {
	Number int
	UIDL   string
}

func (c *pop3Client) UIDL() ([]pop3UIDL, error) {
	if _, err := c.cmd("UIDL"); err != nil {
		return nil, err
	}
	raw, err := c.readMultiline()
	if err != nil {
		return nil, err
	}
	var out []pop3UIDL
	for _, line := range raw {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		num, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		out = append(out, pop3UIDL{Number: num, UIDL: fields[1]})
	}
	return out, nil
}

func (c *pop3Client) Retr(num int) (string, error) {
	if _, err := c.cmd("RETR %d", num); err != nil {
		return "", err
	}
	lines, err := c.readMultiline()
	if err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

// Top retrieves the first lines of a message (headers only for alias indexing).
func (c *pop3Client) Top(num, lines int) (string, error) {
	if lines <= 0 {
		lines = 128
	}
	if _, err := c.cmd("TOP %d %d", num, lines); err != nil {
		return "", err
	}
	raw, err := c.readMultiline()
	if err != nil {
		return "", err
	}
	return strings.Join(raw, "\n"), nil
}
