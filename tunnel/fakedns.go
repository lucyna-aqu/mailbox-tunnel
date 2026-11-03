package tunnel

import (
	"encoding/binary"
	"net/netip"
	"strings"
	"sync"

	"golang.org/x/net/dns/dnsmessage"
)

const fakeDNSTTL = 60

// fakeDNSBase and fakeDNSSize cover 198.18.0.0/15, the range reserved for benchmarking, so it never clashes with real hosts.
var fakeDNSBase = binary.BigEndian.Uint32([]byte{198, 18, 0, 0})

const fakeDNSSize = 1 << 17

// FakeDNS answers A queries at once with an address from 198.18.0.0/15 and remembers which name it stands for.
// A CONNECT to that address is then sent to the node by name, so a lookup costs no round trip through the tunnel.
type FakeDNS struct {
	mutex  sync.Mutex
	byName map[string]netip.Addr
	byAddr map[netip.Addr]string
	next   uint32
}

func NewFakeDNS() *FakeDNS {
	return &FakeDNS{byName: map[string]netip.Addr{}, byAddr: map[netip.Addr]string{}, next: 1}
}

// Lookup returns the name a fake address stands for.
func (f *FakeDNS) Lookup(address netip.Addr) (string, bool) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	name, ok := f.byAddr[address]
	return name, ok
}

func (f *FakeDNS) addressFor(name string) netip.Addr {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if address, ok := f.byName[name]; ok {
		return address
	}
	var raw [4]byte
	binary.BigEndian.PutUint32(raw[:], fakeDNSBase+f.next)
	address := netip.AddrFrom4(raw)
	f.next = f.next%(fakeDNSSize-2) + 1
	if old, ok := f.byAddr[address]; ok {
		delete(f.byName, old) // the range wrapped around; the oldest name loses its address
	}
	f.byName[name], f.byAddr[address] = address, name
	return address
}

// Answer builds the reply to a DNS query: a fake address for A, and an empty answer for everything else,
// so applications fall back to IPv4 and plain A lookups.
func (f *FakeDNS) Answer(query []byte) ([]byte, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return nil, err
	}
	question, err := parser.Question()
	if err != nil {
		return nil, err
	}

	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID:                 header.ID,
		Response:           true,
		RecursionDesired:   header.RecursionDesired,
		RecursionAvailable: true,
	})
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return nil, err
	}
	if err := builder.Question(question); err != nil {
		return nil, err
	}
	if err := builder.StartAnswers(); err != nil {
		return nil, err
	}
	if question.Type == dnsmessage.TypeA && question.Class == dnsmessage.ClassINET {
		name := strings.TrimSuffix(strings.ToLower(question.Name.String()), ".")
		resource := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, TTL: fakeDNSTTL}
		if err := builder.AResource(resource, dnsmessage.AResource{A: f.addressFor(name).As4()}); err != nil {
			return nil, err
		}
	}
	return builder.Finish()
}
