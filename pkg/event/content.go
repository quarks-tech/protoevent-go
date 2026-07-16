package event

import (
	"strings"
)

const (
	baseContentType        = "application"
	cloudeventsContentType = "application/cloudevents"
)

// ContentSubtype extracts the codec-selecting subtype from a content type:
// "application/cloudevents+proto" and "application/proto" both yield
// ("proto", true). Returns ok=false for content types outside the
// application/ (or application/cloudevents+) families, for a bare
// "application"/"application/cloudevents" with no subtype, and for
// non-"application/"-delimited lookalikes ("applicationfoo").
//
// The content type arrives from the INCOMING message's metadata, so this must
// never panic on malformed input: the subscriber maps ok=false to an
// unprocessable-event error instead.
func ContentSubtype(contentType string) (string, bool) {
	if !strings.HasPrefix(contentType, cloudeventsContentType) {
		subtype, ok := strings.CutPrefix(contentType, baseContentType+"/")
		if !ok || subtype == "" {
			return "", false
		}
		return subtype, true
	}

	// Has the cloudevents prefix; an exact match carries no subtype, and
	// indexing past it without the length check would panic on it.
	if len(contentType) == len(cloudeventsContentType) {
		return "", false
	}
	switch contentType[len(cloudeventsContentType)] {
	case '+', ';':
		if rest := contentType[len(cloudeventsContentType)+1:]; rest != "" {
			return rest, true
		}
		return "", false
	default:
		return "", false
	}
}

// ContentType is ContentSubtype's inverse for the plain application/ family:
// ContentType("proto") == "application/proto".
func ContentType(contentSubtype string) string {
	return baseContentType + "/" + contentSubtype
}
