package waversion

import (
	"encoding/hex"
	"testing"
)

func TestParseString(t *testing.T) {
	v, err := Parse("2.3000.1050469598")
	if err != nil {
		t.Fatal(err)
	}
	if v != (Version{2, 3000, 1050469598}) || v.String() != "2.3000.1050469598" {
		t.Fatalf("got %v", v)
	}
	for _, bad := range []string{"", "2.3000", "2.x.1", "1.2.3.4", "2.3000.99999999999"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestLess(t *testing.T) {
	if !(Version{2, 3000, 1}).Less(Version{2, 3000, 2}) || (Version{2, 3001, 0}).Less(Version{2, 3000, 9}) {
		t.Fatal("ordering wrong")
	}
}

func TestBuildHash(t *testing.T) {
	// md5("2.3000.1") computed independently with `md5 -s`.
	h := (Version{2, 3000, 1}).BuildHash()
	if got := hex.EncodeToString(h[:]); got != "236487c02d95b982c2cdc3c6e961e0e3" {
		t.Fatalf("md5 = %s", got)
	}
}

func TestParseServiceWorker(t *testing.T) {
	cases := []string{
		`self.__swData=JSON.parse(/*BTDS*/"{\"dynamic_data\":{\"SiteData\":{\"server_revision\":1,\"client_revision\":1050469598,\"push_phase\":\"C3\"}}}");`,
		`{"client_revision": 1050469598}`,
	}
	for _, c := range cases {
		v, err := parseServiceWorker([]byte(c))
		if err != nil || v != (Version{2, 3000, 1050469598}) {
			t.Fatalf("%s: got %v %v", c, v, err)
		}
	}
	if _, err := parseServiceWorker([]byte("<html>error</html>")); err == nil {
		t.Fatal("expected error")
	}
}
