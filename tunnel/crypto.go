package tunnel

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"
	"golang.org/x/crypto/nacl/box"
)

const nodeScheme = "mtn://"

var base64URL = base64.RawURLEncoding

// NodeInfo is what a node publishes: its registration mailbox and its public key.
type NodeInfo struct {
	DeploymentID string
	AuthKey      string
	PublicKey    [32]byte
}

// String encodes the node as mtn://<deployment>/<key>#<public key>.
func (n NodeInfo) String() string {
	return nodeScheme + n.DeploymentID + "/" + n.AuthKey + "#" + base64URL.EncodeToString(n.PublicKey[:])
}

func ParseNodeInfo(text string) (NodeInfo, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(text), nodeScheme)
	hash := strings.LastIndex(rest, "#")
	if !ok || hash < 0 {
		return NodeInfo{}, errors.New("node string must look like mtn://<deployment>/<key>#<public key>")
	}
	deploymentID, authKey, found := strings.Cut(rest[:hash], "/")
	publicKey, err := base64URL.DecodeString(rest[hash+1:])
	if !found || deploymentID == "" || authKey == "" || err != nil || len(publicKey) != 32 {
		return NodeInfo{}, errors.New("malformed node string")
	}
	info := NodeInfo{DeploymentID: deploymentID, AuthKey: authKey}
	copy(info.PublicKey[:], publicKey)
	return info, nil
}

// generateKeyPair returns a fresh X25519 key pair.
func generateKeyPair() (privateKey, publicKey [32]byte, err error) {
	public, private, err := box.GenerateKey(rand.Reader)
	if err != nil {
		return privateKey, publicKey, err
	}
	return *private, *public, nil
}

// parsePrivateKey decodes a base64url X25519 private key and derives its public key.
func parsePrivateKey(text string) (privateKey, publicKey [32]byte, err error) {
	decoded, err := base64URL.DecodeString(strings.TrimSpace(text))
	if err != nil || len(decoded) != 32 {
		return privateKey, publicKey, errors.New("node key must be 32 bytes of base64url")
	}
	copy(privateKey[:], decoded)
	public, err := curve25519.X25519(privateKey[:], curve25519.Basepoint)
	if err != nil {
		return privateKey, publicKey, err
	}
	copy(publicKey[:], public)
	return privateKey, publicKey, nil
}

// registration tells a node which mailbox to serve. Only the node can open it.
type registration struct {
	DeploymentID string `json:"d"`
	AuthKey      string `json:"k"`
	RunID        string `json:"r"`
	ClientKey    []byte `json:"c"`
}

func sealRegistration(entry registration, nodePublicKey *[32]byte) ([]byte, error) {
	plain, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	return box.SealAnonymous(nil, plain, nodePublicKey, rand.Reader)
}

func openRegistration(sealed []byte, nodePublicKey, nodePrivateKey *[32]byte) (registration, error) {
	var entry registration
	plain, ok := box.OpenAnonymous(nil, sealed, nodePublicKey, nodePrivateKey)
	if !ok {
		return entry, errors.New("registration is not for this node")
	}
	if err := json.Unmarshal(plain, &entry); err != nil {
		return entry, err
	}
	if entry.DeploymentID == "" || entry.AuthKey == "" || entry.RunID == "" || len(entry.ClientKey) != 32 {
		return entry, errors.New("incomplete registration")
	}
	return entry, nil
}

// streamCipher seals the batches of one direction, using the batch sequence number as the nonce.
type streamCipher struct {
	aead cipher.AEAD
}

// newStreamCiphers derives the client-to-node and node-to-client ciphers from an X25519 exchange.
func newStreamCiphers(privateKey *[32]byte, peerPublicKey []byte, runID string) (up, down streamCipher, err error) {
	shared, err := curve25519.X25519(privateKey[:], peerPublicKey)
	if err != nil {
		return up, down, err
	}
	derive := func(label string) (streamCipher, error) {
		key := make([]byte, chacha20poly1305.KeySize)
		if _, err := io.ReadFull(hkdf.New(sha256.New, shared, []byte(runID), []byte(label)), key); err != nil {
			return streamCipher{}, err
		}
		aead, err := chacha20poly1305.New(key)
		return streamCipher{aead: aead}, err
	}
	if up, err = derive("mailbox-tunnel up"); err != nil {
		return up, down, err
	}
	down, err = derive("mailbox-tunnel down")
	return up, down, err
}

func (c streamCipher) nonce(sequence int) []byte {
	nonce := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(nonce[4:], uint64(sequence))
	return nonce
}

func (c streamCipher) seal(sequence int, plain []byte) []byte {
	return c.aead.Seal(nil, c.nonce(sequence), plain, nil)
}

func (c streamCipher) open(sequence int, sealed []byte) ([]byte, error) {
	plain, err := c.aead.Open(nil, c.nonce(sequence), sealed, nil)
	if err != nil {
		return nil, fmt.Errorf("batch %d failed authentication", sequence)
	}
	return plain, nil
}
