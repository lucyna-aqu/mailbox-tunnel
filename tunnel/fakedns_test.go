package tunnel

import (
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

func buildQuery(t *testing.T, name string, queryType dnsmessage.Type) []byte {
	t.Helper()
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 42, RecursionDesired: true})
	builder.StartQuestions()
	builder.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: queryType, Class: dnsmessage.ClassINET})
	query, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return query
}

func parseAnswers(t *testing.T, reply []byte) (dnsmessage.Header, []dnsmessage.Resource) {
	t.Helper()
	var message dnsmessage.Message
	if err := message.Unpack(reply); err != nil {
		t.Fatal(err)
	}
	return message.Header, message.Answers
}

func TestFakeDNSAnswersAWithAStableFakeAddress(t *testing.T) {
	fake := NewFakeDNS()
	first, err := fake.Answer(buildQuery(t, "Example.COM.", dnsmessage.TypeA))
	if err != nil {
		t.Fatal(err)
	}
	header, answers := parseAnswers(t, first)
	if header.ID != 42 || !header.Response || len(answers) != 1 {
		t.Fatalf("unexpected reply: %+v, %d answers", header, len(answers))
	}
	address := netip.AddrFrom4(answers[0].Body.(*dnsmessage.AResource).A)
	if !netip.MustParsePrefix("198.18.0.0/15").Contains(address) {
		t.Fatalf("address %s is outside 198.18.0.0/15", address)
	}
	if name, ok := fake.Lookup(address); !ok || name != "example.com" {
		t.Fatalf("lookup of %s gave %q, %v", address, name, ok)
	}

	again, _ := fake.Answer(buildQuery(t, "example.com.", dnsmessage.TypeA))
	_, answersAgain := parseAnswers(t, again)
	if netip.AddrFrom4(answersAgain[0].Body.(*dnsmessage.AResource).A) != address {
		t.Fatal("the same name got a different address")
	}
}

func TestFakeDNSGivesEmptyAnswerForAAAA(t *testing.T) {
	reply, err := NewFakeDNS().Answer(buildQuery(t, "example.com.", dnsmessage.TypeAAAA))
	if err != nil {
		t.Fatal(err)
	}
	if header, answers := parseAnswers(t, reply); header.RCode != dnsmessage.RCodeSuccess || len(answers) != 0 {
		t.Fatalf("expected an empty success reply, got %v with %d answers", header.RCode, len(answers))
	}
}

func TestSplitSocksDatagram(t *testing.T) {
	packet := []byte{0, 0, 0, 1, 1, 1, 1, 1, 0, 53, 'q', 'u', 'e', 'r', 'y'}
	header, port, payload, ok := splitSocksDatagram(packet)
	if !ok || port != 53 || string(payload) != "query" || len(header) != 10 {
		t.Fatalf("got header %v port %d payload %q ok %v", header, port, payload, ok)
	}
	if _, _, _, ok := splitSocksDatagram([]byte{0, 0, 1, 1, 1, 1, 1, 1, 0, 53}); ok {
		t.Fatal("a fragmented datagram was accepted")
	}
}
