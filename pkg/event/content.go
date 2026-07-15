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
// application/ (or application/cloudevents+) families.
func ContentSubtype(contentType string) (string, bool) {
	if !strings.HasPrefix(contentType, cloudeventsContentType) {
		if strings.HasPrefix(contentType, baseContentType) {
			return contentType[len(baseContentType)+1:], true
		}

		return "", false
	}

	// guaranteed since != cloudeventsContentType and has cloudeventsContentType prefix
	switch contentType[len(cloudeventsContentType)] {
	case '+', ';':
		return contentType[len(cloudeventsContentType)+1:], true
	default:
		return "", false
	}
}

// ContentType is ContentSubtype's inverse for the plain application/ family:
// ContentType("proto") == "application/proto".
func ContentType(contentSubtype string) string {
	return baseContentType + "/" + contentSubtype
}
