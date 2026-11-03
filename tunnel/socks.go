package tunnel

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
)

const (
	socksConnect      byte = 1
	socksUDPAssociate byte = 3
)

// socks5Handshake does a minimal SOCKS5 exchange (no auth) and returns the target as host:port and the command.
// CONNECT is answered with success at once to save a mailbox round trip; UDP ASSOCIATE is left for the caller to answer.
func socks5Handshake(conn net.Conn) (string, byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", 0, err
	}
	if _, err := io.ReadFull(conn, make([]byte, header[1])); err != nil {
		return "", 0, err
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return "", 0, err
	}

	request := make([]byte, 4)
	if _, err := io.ReadFull(conn, request); err != nil {
		return "", 0, err
	}
	command := request[1]
	if command != socksConnect && command != socksUDPAssociate {
		conn.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return "", 0, errors.New("unsupported SOCKS command")
	}
	host, err := readSocksAddress(conn, request[3])
	if err != nil {
		return "", 0, err
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(conn, port); err != nil {
		return "", 0, err
	}
	if command == socksConnect {
		if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return "", 0, err
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port)))), command, nil
}

func readSocksAddress(reader io.Reader, addressType byte) (string, error) {
	switch addressType {
	case 1:
		address := make([]byte, 4)
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	case 3:
		length := make([]byte, 1)
		if _, err := io.ReadFull(reader, length); err != nil {
			return "", err
		}
		name := make([]byte, length[0])
		if _, err := io.ReadFull(reader, name); err != nil {
			return "", err
		}
		return string(name), nil
	case 4:
		address := make([]byte, 16)
		if _, err := io.ReadFull(reader, address); err != nil {
			return "", err
		}
		return net.IP(address).String(), nil
	}
	return "", errors.New("unknown address type")
}

// splitSocksDatagram splits a SOCKS5 UDP datagram into its header (to reuse in the reply), destination port and payload.
func splitSocksDatagram(packet []byte) (header []byte, port uint16, payload []byte, ok bool) {
	if len(packet) < 4 || packet[2] != 0 {
		return nil, 0, nil, false // fragments are not supported
	}
	offset := 4
	switch packet[3] {
	case 1:
		offset += 4
	case 4:
		offset += 16
	case 3:
		if len(packet) < 5 {
			return nil, 0, nil, false
		}
		offset += 1 + int(packet[4])
	default:
		return nil, 0, nil, false
	}
	if len(packet) < offset+2 {
		return nil, 0, nil, false
	}
	port = binary.BigEndian.Uint16(packet[offset : offset+2])
	return packet[:offset+2], port, packet[offset+2:], true
}
