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
	home, _ := os.UserHomeDir()
	for i, tok := range argv {
		if home != "" && strings.HasPrefix(tok, home+string(os.PathSeparator)) {
			argv[i] = "~" + tok[len(home):]
		}
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
