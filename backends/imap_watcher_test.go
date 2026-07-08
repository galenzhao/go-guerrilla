package backends

import "testing"

func TestNewMailUIDRangeNoNewMail(t *testing.T) {
	fromUID, toUID, hasNew := newMailUIDRange(100, 101)
	if hasNew || fromUID != 101 || toUID != 0 {
		t.Fatalf("expected no new mail, got from=%d to=%d hasNew=%v", fromUID, toUID, hasNew)
	}
}

func TestNewMailUIDRangeSomeNewMail(t *testing.T) {
	fromUID, toUID, hasNew := newMailUIDRange(100, 105)
	if !hasNew || fromUID != 101 || toUID != 104 {
		t.Fatalf("unexpected range: from=%d to=%d hasNew=%v", fromUID, toUID, hasNew)
	}
}

func TestNewMailUIDRangeFirstSync(t *testing.T) {
	fromUID, toUID, hasNew := newMailUIDRange(0, 501)
	if !hasNew || fromUID != 1 || toUID != 500 {
		t.Fatalf("unexpected first-sync range: from=%d to=%d hasNew=%v", fromUID, toUID, hasNew)
	}
}

func TestNewMailUIDRangeUnknownUIDNext(t *testing.T) {
	fromUID, toUID, hasNew := newMailUIDRange(50, 0)
	if hasNew || fromUID != 51 || toUID != 0 {
		t.Fatalf("expected no new mail when uidnext is unknown, got from=%d to=%d hasNew=%v", fromUID, toUID, hasNew)
	}
}

func TestNewMailUIDRangeSingleMessage(t *testing.T) {
	fromUID, toUID, hasNew := newMailUIDRange(9, 11)
	if !hasNew || fromUID != 10 || toUID != 10 {
		t.Fatalf("unexpected single-message range: from=%d to=%d hasNew=%v", fromUID, toUID, hasNew)
	}
}

func TestHeaderBlockLooksEmpty(t *testing.T) {
	cases := []struct {
		name string
		h    parsedMailHeaders
		want bool
	}{
		{"all empty", parsedMailHeaders{}, true},
		{"has message-id only", parsedMailHeaders{MessageID: "<a@b>"}, false},
		{"has from only", parsedMailHeaders{From: "a@b.com"}, false},
		{"has to only", parsedMailHeaders{To: "a@b.com"}, false},
		{"has cc but nothing else", parsedMailHeaders{Cc: "a@b.com"}, true},
		{"fully populated", parsedMailHeaders{MessageID: "<a@b>", From: "a@b.com", To: "c@d.com"}, false},
	}
	for _, c := range cases {
		if got := headerBlockLooksEmpty(c.h); got != c.want {
			t.Errorf("%s: headerBlockLooksEmpty() = %v, want %v", c.name, got, c.want)
		}
	}
}
