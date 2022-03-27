package event

import (
	"strings"
)

const (
	baseContentType        = "application"
	cloudeventsContentType = "application/cloudevents"
)

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

func ContentType(contentSubtype string) string {
	return baseContentType + "/" + contentSubtype
}
