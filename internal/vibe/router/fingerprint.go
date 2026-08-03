package router

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
)

// flagsSHA256 is the serving-flags fingerprint (fleet-control C3 §5):
// SHA-256 over the model's rendered serving argv, canonicalized so both
// sides of the fleet derive the same value from the same def. Both the
// cell (announcing what it serves) and fleetd (validating against its
// own render) compute it via modelCmd, so a mismatch means real drift
// on that cell — never a normalization artifact.
//
// Canonicalization: drop argv[0] (binary path — boxes install it
// differently) and the port argument (deployment detail, not a serving
// flag), sort the remaining --flag value pairs lexicographically, join
// with \x00. Tokenization understands the renderer's quoting (double
// quotes with \" and \\ escapes).

// FlagsSHA256 hashes the rendered serving cmd for one model.
func FlagsSHA256(cmd string) (string, error) {
	argv, err := tokenizeCmd(cmd)
	if err != nil {
		return "", err
	}
	pairs := canonicalFlagPairs(argv)
	h := sha256.New()
	for _, p := range pairs {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// canonicalFlagPairs drops argv[0] and --port pairs, normalizes
// home-anchored paths (a def written `~/models/x` renders as /root/...
// on one box and /home/alice/... on another — same def, and the
// fingerprint must agree), then sorts the remaining flag groups. A
// group is a --flag plus its non-flag values (bool flags stand alone).
func canonicalFlagPairs(argv []string) []string {
	if len(argv) == 0 {
		return nil
	}
	argv = argv[1:] // argv[0] is the binary path
	for i, tok := range argv {
		argv[i] = normalizeHome(tok)
	}
	var groups []string
	for i := 0; i < len(argv); {
		tok := argv[i]
		if !strings.HasPrefix(tok, "--") {
			// A value detached from any flag (shouldn't happen with the
			// renderer's output, but don't silently drop data — keep it).
			groups = append(groups, tok)
			i++
			continue
		}
		group := tok
		i++
		for i < len(argv) && !strings.HasPrefix(argv[i], "--") {
			group += " " + argv[i]
			i++
		}
		if strings.HasPrefix(group, "--port ") || group == "--port" {
			continue
		}
		groups = append(groups, group)
	}
	sort.Strings(groups)
	return groups
}

// normalizeHome folds a home-anchored path to "~/…" INDEPENDENTLY of
// which box computes it. Anchoring on the local $HOME alone (fleetd runs
// root, cells run users) made a def carrying a literal /home/<user>/…
// path hash differently on the two sides forever — and on a strict def
// that fail-closed yanks a working model.
//
// The trade, stated plainly: two users' trees on one box now hash
// identically, so this fails OPEN. That is the right bias. A false
// mismatch pulls a healthy model out of the catalog; a false match only
// misses one flavour of drift, and weights-path swaps outside a home
// directory still mismatch.
func normalizeHome(tok string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rest, ok := strings.CutPrefix(tok, home+"/"); ok {
			return "~/" + rest
		}
	}
	if rest, ok := strings.CutPrefix(tok, "/root/"); ok {
		return "~/" + rest
	}
	if rest, ok := strings.CutPrefix(tok, "/home/"); ok {
		// /home/<user>/rest → ~/rest; a bare /home/<user> is not a
		// home-anchored PATH, so leave it alone.
		if i := strings.Index(rest, "/"); i > 0 {
			return "~/" + rest[i+1:]
		}
	}
	if rest, ok := strings.CutPrefix(tok, "/Users/"); ok {
		// macOS cells (the mlx_server class) anchor here.
		if i := strings.Index(rest, "/"); i > 0 {
			return "~/" + rest[i+1:]
		}
	}
	return tok
}

// tokenizeCmd splits a rendered cmd string on whitespace outside double
// quotes, unescaping \" and \\ inside them (the renderer's quoteArg
// format).
func tokenizeCmd(cmd string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inQuote := false
	escaped := false
	flush := func() {
		if cur.Len() > 0 {
			argv = append(argv, cur.String())
			cur.Reset()
		}
	}
	for _, r := range cmd {
		switch {
		case escaped:
			if r != '"' && r != '\\' {
				return nil, fmt.Errorf("invalid escape \\%c in cmd", r)
			}
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && inQuote:
			escaped = true
		case r == '"':
			inQuote = !inQuote
		case (r == ' ' || r == '\t' || r == '\n') && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	if escaped || inQuote {
		return nil, fmt.Errorf("unterminated quote in cmd")
	}
	flush()
	return argv, nil
}
