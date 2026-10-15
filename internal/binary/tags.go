// Package binary implements WhatsApp's binary XML ("WABinary") node format:
// the Node/JID model, token dictionaries, and the encoder/decoder used for
// every stanza exchanged after the Noise handshake.
package binary

// Wire tags. Byte values 1..len(singleByteTokens)-1 are single-byte tokens;
// the values below are structural markers.
const (
	tagListEmpty   = 0
	tagDictionary0 = 236 // followed by one index byte into doubleByteTokens[0]
	tagDictionary1 = 237
	tagDictionary2 = 238
	tagDictionary3 = 239
	tagInteropJID  = 245 // user, device(u16), integrator(u16), server
	tagFBJID       = 246 // user, device(u16), server
	tagADJID       = 247 // agent(u8), device(u8), user  (server implied by agent)
	tagList8       = 248
	tagList16      = 249
	tagJIDPair     = 250 // user (or LIST_EMPTY), server
	tagHex8        = 251 // packed 0-9A-F, two chars per byte
	tagBinary8     = 252
	tagBinary20    = 253
	tagBinary32    = 254
	tagNibble8     = 255 // packed 0-9 '-' '.', two chars per byte
)

// packedMax is the longest string that can be nibble/hex packed: the length
// byte stores ceil(len/2) in 7 bits.
const packedMax = 127

var (
	singleByteIndex = make(map[string]byte, len(singleByteTokens))
	doubleByteIndex = make(map[string][2]byte, 4*256)
)

func init() {
	if len(singleByteTokens) > tagDictionary0 {
		panic("binary: single byte token table overlaps structural tags")
	}
	for i, tok := range singleByteTokens {
		if i != 0 {
			singleByteIndex[tok] = byte(i)
		}
	}
	for d, dict := range doubleByteTokens {
		for i, tok := range dict {
			doubleByteIndex[tok] = [2]byte{byte(d), byte(i)}
		}
	}
}
