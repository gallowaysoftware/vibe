// Package modelcat is the one place vibe knows what a /v1/models catalog
// looks like on the wire.
//
// Two shapes arrive on that path. OpenAI's, which llama-swap, llama.cpp
// and every OpenAI client speak:
//
//	{"object":"list","data":[{"id":"qwen3.6","object":"model"}]}
//
// and Ollama's, which some upstreams emit instead. llama.cpp emits BOTH
// keys; an Ollama-compatible server emits only this one:
//
//	{"models":[{"name":"qwen3.6","model":"qwen3.6","size":…,"digest":…}]}
//
// A catalog is a promise that every id in it is servable, and the whole
// control plane is built on that promise: consumers PIN the ids they read
// here and send them back on /v1/chat/completions. So the two failure
// modes this package exists to prevent are the quiet ones —
//
//   - reading data[] out of an Ollama body and seeing an empty catalog,
//     which advertises a cell as serving nothing while it serves fine
//     (vamp's ResolveModelID goes further and substitutes the literal id
//     "vibe", which is a catalog id that 404s on use), and
//   - forwarding a shape nobody recognised as though it were OpenAI's.
//
// Both are why Parse returns an error for an unreadable body rather than
// an empty catalog: an empty catalog is a claim, and this package must
// not make one it cannot support.
//
// Ollama-only fields (size, digest, modified_at) are dropped on the way
// through. They are facts about a blob on one host's disk, not about what
// is servable, they have no OpenAI counterpart, and a consumer that keyed
// off them would be keying off something that does not survive the trip
// between two cells running different backends.
package modelcat

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrShape reports a body that is not a /v1/models catalog in any shape
// vibe recognises. Callers must surface it: it means the upstream's
// catalog could not be read, NOT that the upstream serves nothing.
var ErrShape = errors.New("unrecognised /v1/models shape")

// Entry is one catalog row in either shape. OpenAI rows carry ID (and
// sometimes a display Name); Ollama rows carry Name and Model and no id
// at all.
type Entry struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Model string `json:"model"`
}

// Wire is a /v1/models body as it arrives, with both shapes' keys decoded
// side by side.
//
// fleetapi reads this rather than a normalised Catalog on purpose: WHICH
// key a cell answered with is residency evidence there — a llama.cpp-family
// upstream advertises only what it has LOADED — so that consumer has to
// see the two shapes apart, where a client-facing catalog must see them
// folded together.
type Wire struct {
	Object string  `json:"object"`
	Data   []Entry `json:"data"`
	Ollama []Entry `json:"models"`
}

// OllamaID is the catalog id an Ollama row advertises. Ollama's own
// clients address `model`; `name` is the fallback for the servers that
// only send that.
func (e Entry) OllamaID() string {
	if e.Model != "" {
		return e.Model
	}
	return e.Name
}

// Catalog is a parsed catalog: the OpenAI rows verbatim and in order,
// indexed by the id a client must send back, plus whether the upstream
// also spoke the Ollama shape and with which ids.
type Catalog struct {
	entries []entry
	index   map[string]int

	// hasOllama records that the upstream's body carried a models[] key,
	// which is a claim about residency and not just about shape. ollama
	// holds the ids it listed.
	hasOllama bool
	ollama    []string
}

type entry struct {
	id  string
	raw json.RawMessage
}

// Parse reads a /v1/models body in either shape. It returns ErrShape when
// the body is not a JSON object carrying a data[] or models[] array —
// including for an upstream that answers /v1/models with something else
// entirely, which must never be forwarded as though it were a catalog.
func Parse(body []byte) (*Catalog, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrShape, err)
	}
	c := &Catalog{index: map[string]int{}}

	var rows []json.RawMessage
	haveData := false
	if raw, ok := top["data"]; ok {
		if json.Unmarshal(raw, &rows) == nil {
			haveData = true
		}
	}
	var oll []Entry
	if raw, ok := top["models"]; ok {
		if json.Unmarshal(raw, &oll) == nil {
			c.hasOllama = true
		}
	}
	if !haveData && !c.hasOllama {
		return nil, ErrShape
	}

	for _, raw := range rows {
		var e Entry
		if json.Unmarshal(raw, &e) != nil || e.ID == "" {
			// A row with no id names nothing a client can send back. It is
			// exactly the phantom this package exists to keep out of the
			// catalog, so it is dropped rather than passed along.
			continue
		}
		c.put(e.ID, raw)
	}
	for _, e := range oll {
		id := e.OllamaID()
		if id == "" {
			continue
		}
		c.addOllama(id)
		c.put(id, synth(id))
	}
	return c, nil
}

// put appends a row, keeping the first row seen for an id.
func (c *Catalog) put(id string, raw json.RawMessage) {
	if _, dup := c.index[id]; dup {
		return
	}
	c.index[id] = len(c.entries)
	c.entries = append(c.entries, entry{id: id, raw: raw})
}

func (c *Catalog) addOllama(id string) {
	for _, have := range c.ollama {
		if have == id {
			return
		}
	}
	c.ollama = append(c.ollama, id)
}

// synth is the OpenAI row for an id that reached us only in the Ollama
// shape. created is 0 — llama-swap's own value for a model it has not
// started — rather than a fabricated timestamp: the field is present
// because strict OpenAI clients require it, not because we know it.
func synth(id string) json.RawMessage {
	b, err := json.Marshal(struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}{ID: id, Object: "model", OwnedBy: "vibe"})
	if err != nil {
		// Unreachable: the value is four strings and an int.
		return json.RawMessage(`{"id":"","object":"model"}`)
	}
	return b
}

// Rename replaces catalog id from with to, in the OpenAI rows and in the
// Ollama residency list alike. Both have to move together: a rename that
// reached only one of them would publish a catalog whose two halves
// disagree about what the cell is called.
func (c *Catalog) Rename(from, to string) {
	if from == "" || to == "" || from == to {
		return
	}
	if _, ok := c.index[from]; ok {
		old := c.entries
		c.entries = nil
		c.index = map[string]int{}
		for _, e := range old {
			if e.id == from {
				e.id, e.raw = to, withID(e.raw, to)
			}
			c.put(e.id, e.raw)
		}
	}
	// Rebuilt through addOllama rather than patched in place: renaming onto
	// an id the list already holds would otherwise leave it there twice.
	old := c.ollama
	c.ollama = nil
	for _, id := range old {
		if id == from {
			id = to
		}
		c.addOllama(id)
	}
}

// withID returns raw with its "id" set to id, leaving every other field
// of the row untouched.
func withID(raw json.RawMessage, id string) json.RawMessage {
	var row map[string]json.RawMessage
	if json.Unmarshal(raw, &row) != nil {
		return synth(id)
	}
	repl, err := json.Marshal(id)
	if err != nil {
		return synth(id)
	}
	row["id"] = repl
	out, err := json.Marshal(row)
	if err != nil {
		return synth(id)
	}
	return out
}

// Merge folds other into c, keeping c's row for any id both carry.
func (c *Catalog) Merge(other *Catalog) {
	if other == nil {
		return
	}
	for _, e := range other.entries {
		c.put(e.id, e.raw)
	}
	if other.hasOllama {
		c.hasOllama = true
		for _, id := range other.ollama {
			c.addOllama(id)
		}
	}
}

// IDs are the catalog ids, in catalog order.
func (c *Catalog) IDs() []string {
	out := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.id)
	}
	return out
}

// Has reports whether id is in the catalog.
func (c *Catalog) Has(id string) bool {
	_, ok := c.index[id]
	return ok
}

// OllamaIDs are the ids the upstream advertised under models[], and nil
// when it did not speak that shape at all.
func (c *Catalog) OllamaIDs() []string { return c.ollama }

type ollamaRow struct {
	Name  string `json:"name"`
	Model string `json:"model"`
}

// JSON renders the catalog in the OpenAI shape: {"object":"list","data":
// [...]}, with every row carrying an id a client can send back.
//
// The Ollama models[] key is re-emitted, ids only, when and only when the
// upstream sent one. Dropping it would be a cleaner shape and a worse
// answer: fleetd reads the key's PRESENCE as residency evidence for a
// cell with no /running to merge against (a llama.cpp-family upstream
// advertises only what it has loaded), and a catalog that stops carrying
// it turns every loaded model on such a cell into a "stopped" one in the
// fleet view. Real llama.cpp emits both keys, so this is a shape that
// already exists on the wire rather than a vibe invention — and emitting
// both halves from ONE id list is what keeps them from ever disagreeing.
func (c *Catalog) JSON() ([]byte, error) {
	rows := make([]json.RawMessage, 0, len(c.entries))
	for _, e := range c.entries {
		rows = append(rows, e.raw)
	}
	out := struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
		Models *[]ollamaRow      `json:"models,omitempty"`
	}{Object: "list", Data: rows}
	if c.hasOllama {
		oll := make([]ollamaRow, 0, len(c.ollama))
		for _, id := range c.ollama {
			oll = append(oll, ollamaRow{Name: id, Model: id})
		}
		out.Models = &oll
	}
	return json.Marshal(out)
}

// Normalize is Parse followed by JSON: an OpenAI-shaped catalog carrying
// every id the body advertised, in either shape.
func Normalize(body []byte) ([]byte, error) {
	c, err := Parse(body)
	if err != nil {
		return nil, err
	}
	return c.JSON()
}
