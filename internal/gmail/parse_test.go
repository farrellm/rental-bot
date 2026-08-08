package gmail

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParsePlainMessage(t *testing.T) {
	raw := "From: Me <me@example.com>\r\n" +
		"To: bot@example.com\r\n" +
		"Subject: Rent for April\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		"Deposited today.\r\n"

	msg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Subject != "Rent for April" {
		t.Errorf("Subject = %q", msg.Subject)
	}
	if !strings.Contains(msg.Text, "Deposited today.") {
		t.Errorf("Text = %q", msg.Text)
	}
	if len(msg.Attachments) != 0 {
		t.Errorf("found %d attachments in a plain message", len(msg.Attachments))
	}
}

// Neither net/mail nor mime/multipart decodes base64, and MIME wraps it at 76
// columns. Getting this wrong stores a PDF as its own base64 text.
func TestBase64AttachmentIsDecoded(t *testing.T) {
	content := strings.Repeat("PDF bytes, and plenty of them. ", 40)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))

	// Wrap it the way a real mail client does.
	var wrapped strings.Builder
	for i := 0; i < len(encoded); i += 76 {
		wrapped.WriteString(encoded[i:min(i+76, len(encoded))])
		wrapped.WriteString("\r\n")
	}

	raw := "From: me@example.com\r\n" +
		"Subject: Receipt\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"b\"\r\n\r\n" +
		"--b\r\nContent-Type: text/plain\r\n\r\nSee attached.\r\n" +
		"--b\r\nContent-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=\"receipt.pdf\"\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		wrapped.String() +
		"--b--\r\n"

	msg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("found %d attachments, want 1", len(msg.Attachments))
	}
	if got := string(msg.Attachments[0].Content); got != content {
		t.Errorf("the attachment did not round trip: %d bytes, want %d", len(got), len(content))
	}
}

// A forwarded message with an attachment is a multipart/mixed containing a
// multipart/alternative. One level of parsing finds nothing.
func TestNestedMultipartIsWalked(t *testing.T) {
	raw := "From: me@example.com\r\n" +
		"Subject: Fwd: invoice\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: multipart/mixed; boundary=\"outer\"\r\n\r\n" +
		"--outer\r\n" +
		"Content-Type: multipart/alternative; boundary=\"inner\"\r\n\r\n" +
		"--inner\r\nContent-Type: text/plain\r\n\r\nThe plain version.\r\n" +
		"--inner\r\nContent-Type: text/html\r\n\r\n<p>The HTML version.</p>\r\n" +
		"--inner--\r\n" +
		"--outer\r\n" +
		"Content-Type: application/pdf\r\n" +
		"Content-Disposition: attachment; filename=\"invoice.pdf\"\r\n\r\n" +
		"raw pdf bytes\r\n" +
		"--outer--\r\n"

	msg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(msg.Text, "The plain version.") {
		t.Errorf("Text = %q, want the text/plain alternative", msg.Text)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("found %d attachments, want 1", len(msg.Attachments))
	}
	if msg.Attachments[0].Filename != "invoice.pdf" {
		t.Errorf("filename = %q", msg.Attachments[0].Filename)
	}
	// The nested part numbering reads like Gmail's own.
	if msg.Attachments[0].PartID != "2" {
		t.Errorf("part id = %q, want 2", msg.Attachments[0].PartID)
	}
}

func TestEncodedHeadersAreDecoded(t *testing.T) {
	raw := "From: =?utf-8?B?Sm9zw6kgUGzDvG1iZXI=?= <jose@example.com>\r\n" +
		"Subject: =?utf-8?q?Reparaci=C3=B3n_del_ba=C3=B1o?=\r\n" +
		"Content-Type: text/plain\r\n\r\nbody\r\n"

	msg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if msg.Subject != "Reparación del baño" {
		t.Errorf("Subject = %q, want the decoded encoded-word", msg.Subject)
	}
	if got := SenderAddress(msg.From); got != "jose@example.com" {
		t.Errorf("SenderAddress = %q", got)
	}
}

func TestQuotedPrintableBody(t *testing.T) {
	raw := "From: me@example.com\r\n" +
		"Subject: Repair\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n" +
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
		"The cost was =C2=A3120 all in.\r\n"

	msg, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !strings.Contains(msg.Text, "£120") {
		t.Errorf("Text = %q, want the decoded quoted-printable", msg.Text)
	}
}

func TestSenderAddress(t *testing.T) {
	tests := []struct {
		name string
		from string
		want string
	}{
		{"bare", "me@example.com", "me@example.com"},
		{"display name", "Me <Me@Example.COM>", "me@example.com"},
		{"quoted display name", `"Ace Hardware, Inc." <billing@ace.example>`, "billing@ace.example"},
		{"malformed but bracketed", "Broken <me@example.com", "broken <me@example.com"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SenderAddress(tt.from); got != tt.want {
				t.Errorf("SenderAddress(%q) = %q, want %q", tt.from, got, tt.want)
			}
		})
	}
}

func TestParseRefusesRubbish(t *testing.T) {
	if _, err := Parse([]byte("this is not an email")); err == nil {
		t.Fatal("Parse accepted something that is not a message")
	}
}
