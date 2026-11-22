package herdrlink

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The owner's core id never reaches the environment, and nothing from the
// environment names the owner's own terminals (review 3, M2): Herdr's core
// turns "<its id>/t1" and "<its name>/t1" from a machine into its own t1
// (remote.rs localize_in), in replies and in the events it mirrors.

var testIDKey = []byte("an environment's own key")

func identityRig(t *testing.T) *rig {
	t.Helper()
	return framedRig(t, Options{IDKey: testIDKey})
}

func TestTheOwnersIDBecomesAPseudonym(t *testing.T) {
	r := identityRig(t)
	pseudonym := Pseudonym(testIDKey, ownerID)
	if pseudonym == ownerID || len(pseudonym) != 32 || Pseudonym(testIDKey, ownerID) != pseudonym || Pseudonym([]byte("another"), ownerID) == pseudonym {
		t.Fatalf("pseudonym %q", pseudonym)
	}
	r.owner(call(8, "terminal.run", "{\"argv\":[\"sh\"],\"caller\":\""+ownerID+"/t3\"}"))
	r.owner(call(9, "terminal.wait", "{\"terminal\":\""+ownerID+"/t3\",\"children\":\"any\",\"caller\":\""+ownerID+"/t3\"}"))
	r.owner(Frame(0, KindData, []byte("{\"jsonrpc\":\"2.0\",\"method\":\"$/orphaned\",\"params\":{\"parent\":\""+ownerID+"/t3\"}}")))
	got := await(t, "the calls at the guest", r.toGuest, func(b []byte) bool { return strings.Contains(string(b), "$/orphaned") })
	if strings.Contains(string(got), ownerID) {
		t.Fatalf("the owner's core id reached the environment: %q", got)
	}
	for _, want := range []string{
		"\"id\":\"" + pseudonym + "\"",
		"\"caller\":\"" + pseudonym + "/t3\"",
		"\"terminal\":\"" + pseudonym + "/t3\"",
		"\"parent\":\"" + pseudonym + "/t3\"",
	} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("no %s at the guest: %q", want, got)
		}
	}
}

func TestNothingFromTheEnvironmentNamesTheOwnersTerminals(t *testing.T) {
	r := identityRig(t)
	pseudonym := Pseudonym(testIDKey, ownerID)
	event := func(kind, terminal string) string {
		return `{"jsonrpc":"2.0","method":"event","params":{"type":"` + kind + `","terminal":"` + terminal + `","data":{"exit_code":0}}}` + "\n"
	}
	var stream []byte
	for _, line := range []string{
		event("created", ownerID+"/t1"),
		event("finished", ownerID+"/t1"),
		event("finished", "owners-macbook-pro/t1"),
		`{"jsonrpc":"2.0","method":"missed","params":{"terminals":[{"address":"t2","parent":ownerID+"/t1"}]}}` + "\n",
		"not json\n",
		// The environment's own terminals, and the owner's as it calls them.
		event("finished", "t5"),
		event("created", pseudonym+"/t1"),
		event("created", "super-cage/t1"),
	} {
		stream = append(stream, line...)
	}
	// Cut anywhere: lines are whole only at the owner.
	for start := 0; start < len(stream); start += 7 {
		r.guest(Frame(1, KindData, stream[start:min(start+7, len(stream))]))
	}
	r.owner(call(8, "terminal.get", "{\"terminal\":\"t5\"}"))
	r.owner(call(10, "terminal.list", "{}"))
	await(t, "the calls at the guest", r.toGuest, func(b []byte) bool { return strings.Contains(string(b), "terminal.list") })
	r.guest(Frame(0, KindData, []byte(`{"jsonrpc":"2.0","id":8,"result":{"address":"t5","parent":ownerID+"/t1"}}`)))
	r.guest(Frame(0, KindData, []byte(`{"jsonrpc":"2.0","id":10,"result":{"terminals":[{"address":"t5","parent":"super-cage/t1"}]}}`)))
	await(t, "the replies", r.toOwner, func(b []byte) bool { return strings.Contains(string(b), "\"id\":10") })
	out := r.toOwner.bytes()
	view := readAsOwner(out, 2)
	var seen []string
	for _, e := range view.events {
		params := serdeObject(serdeField(e, "params"))
		terminal, _ := params["terminal"].(string)
		seen = append(seen, serdeField(e, "method").(string)+":"+terminal)
	}
	if want := []string{"event:t5", "event:" + pseudonym + "/t1", "event:super-cage/t1"}; !slices.Equal(seen, want) {
		t.Fatalf("events the owner's core applied: %q, want %q", seen, want)
	}
	if bytes.Contains(out, []byte(ownerID)) || bytes.Contains(out, []byte("owners-macbook-pro")) {
		t.Fatalf("an address of the owner's reached it: %q", out)
	}
	var replies []map[string]any
	for _, f := range r.ownerFrames() {
		if f.channel == 0 {
			var reply map[string]any
			if json.Unmarshal(f.payload, &reply) != nil {
				t.Fatalf("not JSON: %q", f.payload)
			}
			replies = append(replies, reply)
		}
	}
	last := replies[len(replies)-2:]
	if last[0]["id"] != float64(8) || last[0]["error"] == nil || last[0]["result"] != nil {
		t.Fatalf("a reply naming the owner's terminal passed: %v", last[0])
	}
	if last[1]["id"] != float64(10) || last[1]["result"] == nil {
		t.Fatalf("a reply naming the environment's own parent did not pass: %v", last[1])
	}
}
