package event

import (
	"strings"
)

const baseContentType = "application/cloudevents"

func ContentSubtype(contentType string) (string, bool) {
	if contentType == baseContentType {
		return "", true
	}
	if !strings.HasPrefix(contentType, baseContentType) {
		return "", false
	}
	// guaranteed since != baseContentType and has baseContentType prefix
	switch contentType[len(baseContentType)] {
	case '+', ';':
		return contentType[len(baseContentType)+1:], true
	default:
		return "", false
	}
}

func ContentType(contentSubtype string) string {
	if contentSubtype == "" {
		return baseContentType
	}
	return baseContentType + "+" + contentSubtype
}
