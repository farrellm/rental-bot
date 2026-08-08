package gmail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"path/filepath"
	"strconv"
	"strings"
)

// maxBodyBytes bounds the text body kept in memory. The body is a snippet for
// the register at M3 and the model's input at M4; neither wants a megabyte of
// quoted reply chain.
const maxBodyBytes = 256 << 10

// Message is a parsed email.
type Message struct {
	From        string
	To          string
	Subject     string
	Text        string
	Attachments []Attachment
}

// Attachment is one part worth filing.
type Attachment struct {
	// PartID is the part's position in the message, "1", "2.1" and so on. It is
	// what makes a re-sync write nothing twice, even when two attachments share
	// a filename.
	PartID   string
	Filename string
	MIME     string
	Content  []byte
}

// Parse reads a raw RFC 822 message.
//
// Everything here is untrusted input: a forwarded PDF is written by whoever
// sent it. So the parse is bounded at every step, and a part that will not
// decode is skipped rather than aborting the message — a receipt with one
// broken inline image is still a receipt.
func Parse(raw []byte) (Message, error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return Message{}, fmt.Errorf("gmail: parse message: %w", err)
	}

	decoder := &mime.WordDecoder{}
	header := func(name string) string {
		value := parsed.Header.Get(name)
		decoded, err := decoder.DecodeHeader(value)
		if err != nil {
			// An unparseable encoded-word is better shown raw than dropped.
			return value
		}
		return decoded
	}

	msg := Message{
		From:    header("From"),
		To:      header("To"),
		Subject: header("Subject"),
	}

	mediaType, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil {
		// No Content-Type, or one that will not parse. Treat the whole body as
		// text, which is what a plain message without the header is.
		mediaType, params = "text/plain", map[string]string{}
	}

	if !strings.HasPrefix(mediaType, "multipart/") {
		body, err := decodeBody(parsed.Body, parsed.Header.Get("Content-Transfer-Encoding"))
		if err != nil {
			return msg, fmt.Errorf("gmail: read body: %w", err)
		}
		if strings.HasPrefix(mediaType, "text/") {
			msg.Text = string(body)
		}
		return msg, nil
	}

	if params["boundary"] == "" {
		return msg, errors.New("gmail: multipart message with no boundary")
	}
	if err := walk(&msg, multipart.NewReader(parsed.Body, params["boundary"]), ""); err != nil {
		return msg, err
	}
	return msg, nil
}

// walk descends one multipart level, collecting text and attachments.
//
// prefix numbers the parts the way Gmail's own part ids read: "1", "2",
// "2.1". Nesting is real — a forwarded message with an attachment is a
// multipart/mixed containing a multipart/alternative — so this recurses rather
// than assuming one level.
func walk(msg *Message, reader *multipart.Reader, prefix string) error {
	for index := 1; ; index++ {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// A truncated or malformed boundary. Everything read so far stands;
			// this is the point past which nothing more can be trusted.
			return fmt.Errorf("gmail: read part %s: %w", partID(prefix, index), err)
		}

		id := partID(prefix, index)
		mediaType, params, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			mediaType, params = "application/octet-stream", map[string]string{}
		}

		if strings.HasPrefix(mediaType, "multipart/") && params["boundary"] != "" {
			if err := walk(msg, multipart.NewReader(part, params["boundary"]), id); err != nil {
				part.Close()
				return err
			}
			part.Close()
			continue
		}

		filename := attachmentName(part, params)
		disposition, _, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))

		// Text with no filename and no attachment disposition is the body.
		if filename == "" && disposition != "attachment" && strings.HasPrefix(mediaType, "text/") {
			body, err := decodeBody(io.LimitReader(part, maxBodyBytes), part.Header.Get("Content-Transfer-Encoding"))
			part.Close()
			if err != nil {
				continue
			}
			// text/plain wins over text/html: a multipart/alternative carries
			// both, and the plain one is what a model and a person both want.
			if msg.Text == "" || mediaType == "text/plain" {
				msg.Text = string(body)
			}
			continue
		}

		content, err := decodeBody(part, part.Header.Get("Content-Transfer-Encoding"))
		part.Close()
		if err != nil {
			// One part that will not decode does not invalidate the message.
			continue
		}
		if len(content) == 0 {
			continue
		}
		if filename == "" {
			filename = "part-" + id + extensionFor(mediaType)
		}

		msg.Attachments = append(msg.Attachments, Attachment{
			PartID:   id,
			Filename: filepath.Base(filename),
			MIME:     mediaType,
			Content:  content,
		})
	}
}

// attachmentName reads a filename off a part, preferring the disposition's
// over the content type's.
func attachmentName(part *multipart.Part, typeParams map[string]string) string {
	if name := part.FileName(); name != "" {
		return decodeWord(name)
	}
	if name := typeParams["name"]; name != "" {
		return decodeWord(name)
	}
	return ""
}

func decodeWord(value string) string {
	decoded, err := (&mime.WordDecoder{}).DecodeHeader(value)
	if err != nil {
		return value
	}
	return decoded
}

// decodeBody undoes the transfer encoding.
//
// It is worth being precise about what net/mail and mime/multipart already do,
// because the two differ. multipart.Part decodes quoted-printable transparently
// and hides the header when it does — so the case below never fires for a part,
// and does fire for a non-multipart body, which is exactly right. Neither
// decodes base64, ever. Skipping it stores a PDF as its base64 text: a file a
// third larger than it should be that opens in nothing.
func decodeBody(r io.Reader, encoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "quoted-printable":
		r = quotedprintable.NewReader(r)
	case "base64":
		// MIME wraps base64 at 76 columns, and the decoder rejects the line
		// breaks, so the whitespace comes out first.
		r = base64.NewDecoder(base64.StdEncoding, &unwrapReader{r: r})
	}
	return io.ReadAll(r)
}

// unwrapReader drops ASCII whitespace, so wrapped base64 decodes.
type unwrapReader struct {
	r io.Reader
}

func (u *unwrapReader) Read(p []byte) (int, error) {
	n, err := u.r.Read(p)
	if n == 0 {
		return 0, err
	}
	kept := 0
	for i := range n {
		switch p[i] {
		case '\r', '\n', ' ', '\t':
		default:
			p[kept] = p[i]
			kept++
		}
	}
	// A read that was all whitespace is not the end of the stream. Reporting
	// zero bytes with a nil error is legal but drives io.ReadAll in circles, so
	// this recurses rather than returning it.
	if kept == 0 && err == nil {
		return u.Read(p)
	}
	return kept, err
}

// partID renders "2.1" from a prefix and an index.
func partID(prefix string, index int) string {
	if prefix == "" {
		return strconv.Itoa(index)
	}
	return prefix + "." + strconv.Itoa(index)
}

// extensionFor names a file that arrived without a filename.
func extensionFor(mediaType string) string {
	if exts, err := mime.ExtensionsByType(mediaType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

// SenderAddress extracts the bare address from a From header.
//
// The allowlist compares addresses, not display names: "Ace Hardware
// <billing@ace.example>" and "billing@ace.example" are the same sender, and a
// display name is set by whoever sent the mail.
func SenderAddress(from string) string {
	if addr, err := mail.ParseAddress(from); err == nil {
		return strings.ToLower(strings.TrimSpace(addr.Address))
	}
	// Not a parseable header. Fall back to whatever looks like an address, so a
	// malformed From does not silently match nothing.
	if start := strings.LastIndex(from, "<"); start >= 0 {
		if end := strings.Index(from[start:], ">"); end > 0 {
			return strings.ToLower(strings.TrimSpace(from[start+1 : start+end]))
		}
	}
	return strings.ToLower(strings.TrimSpace(from))
}
