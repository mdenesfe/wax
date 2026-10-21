package client

import "github.com/mdenesfe/wax/proto/wae2e"

// ContextOf returns the context info (reply, mentions, forwarding) of a
// message's content, or nil.
func ContextOf(m *wae2e.Message) *wae2e.ContextInfo {
	switch {
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetContextInfo()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetContextInfo()
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage().GetContextInfo()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetContextInfo()
	case m.GetStickerMessage() != nil:
		return m.GetStickerMessage().GetContextInfo()
	}
	return nil
}

// SetContext sets the context info of m's content; plain text becomes an
// extended text message to carry it. It reports false when the content
// cannot carry one.
func SetContext(m *wae2e.Message, ci *wae2e.ContextInfo) bool {
	switch {
	case m.Conversation != nil:
		if ci != nil {
			m.ExtendedTextMessage = &wae2e.ExtendedTextMessage{Text: m.Conversation, ContextInfo: ci}
			m.Conversation = nil
		}
	case m.ExtendedTextMessage != nil:
		m.ExtendedTextMessage.ContextInfo = ci
	case m.ImageMessage != nil:
		m.ImageMessage.ContextInfo = ci
	case m.VideoMessage != nil:
		m.VideoMessage.ContextInfo = ci
	case m.AudioMessage != nil:
		m.AudioMessage.ContextInfo = ci
	case m.DocumentMessage != nil:
		m.DocumentMessage.ContextInfo = ci
	case m.StickerMessage != nil:
		m.StickerMessage.ContextInfo = ci
	default:
		return false
	}
	return true
}
