package wabin

import (
	"errors"
	"strconv"
)

// Errors returned by the encoder and decoder.
var (
	ErrInvalidType   = errors.New("wabin: unsupported value type")
	ErrInvalidToken  = errors.New("wabin: invalid token")
	ErrInvalidNode   = errors.New("wabin: invalid node")
	ErrInvalidJID    = errors.New("wabin: invalid jid")
	ErrNonStringKey  = errors.New("wabin: non-string attribute key")
	ErrTooDeep       = errors.New("wabin: node nesting too deep")
	ErrTrailingBytes = errors.New("wabin: trailing bytes after node")
	ErrTooLarge      = errors.New("wabin: value too large")
)

// Attrs holds node attributes. Decoded values are string or JID. When
// encoding, integer and bool values are also accepted and written as their
// decimal/textual form; nil and "" values are omitted.
type Attrs map[string]any

// CopyFrom copies the listed keys that are present in src into a and
// returns a, for building replies that echo routing attributes.
func (a Attrs) CopyFrom(src Attrs, keys ...string) Attrs {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			a[k] = v
		}
	}
	return a
}

// Node is one binary XML element.
//
// Content is one of: nil, []Node (children), []byte (binary payload) or
// string (only when the server sent a dictionary token or packed string as
// content). JID content is also possible but rare.
type Node struct {
	Tag     string
	Attrs   Attrs
	Content any
}

// All read accessors (Children, Child, ChildrenByTag, HasChild, Bytes and
// the Attr* methods) accept a nil node, which has no attributes and no
// content, so a missing child (`c, _ := n.Child(...)`) can be read without
// a nil check.

func (n *Node) content() any {
	if n == nil {
		return nil
	}
	return n.Content
}

func (n *Node) attr(key string) any {
	if n == nil {
		return nil
	}
	return n.Attrs[key]
}

// Children returns the child nodes, or nil if the content is not a node
// list.
func (n *Node) Children() []Node {
	c, _ := n.content().([]Node)
	return c
}

// HasChild reports whether a child with the given tag exists.
func (n *Node) HasChild(tag string) bool {
	_, ok := n.Child(tag)
	return ok
}

// Child returns the first child with the given tag.
func (n *Node) Child(tag string) (*Node, bool) {
	children := n.Children()
	for i := range children {
		if children[i].Tag == tag {
			return &children[i], true
		}
	}
	return nil, false
}

// ChildrenByTag returns all children with the given tag.
func (n *Node) ChildrenByTag(tag string) []Node {
	var out []Node
	for _, c := range n.Children() {
		if c.Tag == tag {
			out = append(out, c)
		}
	}
	return out
}

// Bytes returns the content as bytes. String content is converted, because
// the server may tokenize short binary-looking values.
func (n *Node) Bytes() ([]byte, bool) {
	switch c := n.content().(type) {
	case []byte:
		return c, true
	case string:
		return []byte(c), true
	}
	return nil, false
}

// AttrString returns the attribute as a string. JID values are rendered with
// JID.String. The bool is false if the attribute is absent.
func (n *Node) AttrString(key string) (string, bool) {
	switch v := n.attr(key).(type) {
	case string:
		return v, true
	case JID:
		return v.String(), true
	case nil:
		return "", false
	default:
		return attrToString(v)
	}
}

// AttrJID returns the attribute as a JID, parsing it if it was sent as a string.
func (n *Node) AttrJID(key string) (JID, bool) {
	switch v := n.attr(key).(type) {
	case JID:
		return v, true
	case string:
		j, err := ParseJID(v)
		return j, err == nil
	}
	return JID{}, false
}

// AttrInt returns the attribute parsed as a base-10 integer.
func (n *Node) AttrInt(key string) (int64, bool) {
	s, ok := n.AttrString(key)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	return v, err == nil
}

func attrToString(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case int:
		return strconv.Itoa(x), true
	case int32:
		return strconv.FormatInt(int64(x), 10), true
	case int64:
		return strconv.FormatInt(x, 10), true
	case uint:
		return strconv.FormatUint(uint64(x), 10), true
	case uint16:
		return strconv.FormatUint(uint64(x), 10), true
	case uint32:
		return strconv.FormatUint(uint64(x), 10), true
	case uint64:
		return strconv.FormatUint(x, 10), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}
