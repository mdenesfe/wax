package wabin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

// FormatOptions controls how nodes are rendered for logs.
type FormatOptions struct {
	// Mask hides data that must not end up in logs: binary payloads (keys,
	// ciphertexts, pairing refs) are replaced by their length and a short
	// hash prefix, and JID users (phone numbers, LIDs) keep only their last
	// 4 characters. Masked output stays useful for correlating log lines.
	Mask bool
	// MaxBytes limits how much of an unmasked binary payload is printed.
	MaxBytes int
}

// String renders the node as masked XML-like text, safe for logs.
func (n Node) String() string {
	return n.Format(FormatOptions{Mask: true})
}

// Format renders the node as indented XML-like text.
func (n Node) Format(opts FormatOptions) string {
	var b strings.Builder
	n.format(&b, opts, 0)
	return b.String()
}

func (n Node) format(b *strings.Builder, opts FormatOptions, indent int) {
	pad := strings.Repeat("  ", indent)
	b.WriteString(pad)
	b.WriteByte('<')
	b.WriteString(n.Tag)
	keys := make([]string, 0, len(n.Attrs))
	for k := range n.Attrs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		fmt.Fprintf(b, " %s=%q", k, formatAttr(n.Attrs[k], opts))
	}
	switch c := n.Content.(type) {
	case nil:
		b.WriteString("/>")
	case []Node:
		if len(c) == 0 {
			b.WriteString("></")
			b.WriteString(n.Tag)
			b.WriteByte('>')
			return
		}
		b.WriteString(">\n")
		for _, child := range c {
			child.format(b, opts, indent+1)
			b.WriteByte('\n')
		}
		b.WriteString(pad)
		b.WriteString("</")
		b.WriteString(n.Tag)
		b.WriteByte('>')
	default:
		b.WriteByte('>')
		b.WriteString(formatContent(c, opts))
		b.WriteString("</")
		b.WriteString(n.Tag)
		b.WriteByte('>')
	}
}

func formatAttr(v any, opts FormatOptions) string {
	switch x := v.(type) {
	case JID:
		if opts.Mask {
			x.User = maskTail(x.User)
		}
		return x.String()
	case string:
		return x
	}
	s, ok := attrToString(v)
	if !ok {
		return fmt.Sprintf("%v", v)
	}
	return s
}

func formatContent(c any, opts FormatOptions) string {
	switch x := c.(type) {
	case []byte:
		if opts.Mask {
			sum := sha256.Sum256(x)
			return fmt.Sprintf("<!-- %d bytes sha256:%s -->", len(x), hex.EncodeToString(sum[:4]))
		}
		limit := opts.MaxBytes
		if limit <= 0 {
			limit = 1024
		}
		if len(x) > limit {
			return fmt.Sprintf("%s…(+%d bytes)", printable(x[:limit]), len(x)-limit)
		}
		return printable(x)
	case string:
		return x
	case JID:
		return formatAttr(x, opts)
	}
	return fmt.Sprintf("%v", c)
}

// printable shows UTF-8 text as-is and anything else as hex.
func printable(b []byte) string {
	if utf8.Valid(b) && !slices.ContainsFunc(b, func(c byte) bool { return c < 0x20 && c != '\n' && c != '\t' }) {
		return string(b)
	}
	return "0x" + hex.EncodeToString(b)
}

func maskTail(s string) string {
	const keep = 4
	if len(s) <= keep {
		return s
	}
	return strings.Repeat("*", len(s)-keep) + s[len(s)-keep:]
}

// MaskJID renders a JID with all but the last 4 characters of the user
// replaced, for logs.
func MaskJID(j JID) string {
	if j.IsEmpty() {
		return ""
	}
	j.User = maskTail(j.User)
	return j.String()
}
