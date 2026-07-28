package event

import (
	"fmt"
	"strings"
)

const (
	baseContentType        = "application"
	cloudeventsContentType = "application/cloudevents"
)

// ContentSubtype extracts the codec-selecting subtype from a content type:
// "application/cloudevents+proto", "application/cloudevents;proto", and
// "application/proto" all yield ("proto", true). Returns ok=false for content
// types outside the application/ family, for a bare
// "application"/"application/cloudevents" with no subtype, and for
// non-"application/"-delimited lookalikes ("applicationfoo").
//
// RFC 2045 media-type parameters are stripped, so
// "application/cloudevents+json; charset=utf-8" also yields ("json", true) — a
// parameter is part of the media type, not of the codec name, and leaving it
// attached produces a codec name no registered codec matches.
//
// Matching is case-insensitive and the returned subtype is lowercased, per RFC
// 2045 §5.1 (media types are case-insensitive) and to match the codec registry,
// which stores every codec under a lowercased name and documents lookup as
// lowercase. Without this, "application/JSON" resolves to no codec at all and the
// delivery is rejected as unprocessable, even though its payload is ordinary JSON.
//
// The content type arrives from the INCOMING message's metadata, so this must
// never panic on malformed input: the subscriber maps ok=false to an
// unprocessable-event error instead.
func ContentSubtype(contentType string) (string, bool) {
	// Normalize once, up front: every prefix comparison below is then
	// case-insensitive, and the subtype comes out in the registry's canonical form.
	contentType = strings.ToLower(contentType)

	if !strings.HasPrefix(contentType, cloudeventsContentType) {
		subtype, ok := strings.CutPrefix(contentType, baseContentType+"/")
		if !ok {
			return "", false
		}
		return trimMediaTypeParams(subtype)
	}

	// Has the cloudevents prefix; an exact match carries no subtype, and
	// indexing past it without the length check would panic on it.
	if len(contentType) == len(cloudeventsContentType) {
		return "", false
	}
	switch contentType[len(cloudeventsContentType)] {
	case '+':
		return trimMediaTypeParams(contentType[len(cloudeventsContentType)+1:])
	case ';':
		// The structured-mode spelling "application/cloudevents;json" puts the
		// subtype where a media-type parameter would normally sit, so the FIRST
		// ';'-separated field can be the subtype here. But it can equally be an
		// ordinary parameter — "application/cloudevents; charset=utf-8" — and
		// returning "charset=utf-8" as a codec name resolves to no codec while
		// reporting ok=true, so the caller gets an unprocessable-event error instead
		// of the honest "this content type names no subtype". trimMediaTypeParams
		// rejects it: a parameter carries '=', which the token grammar excludes.
		return trimMediaTypeParams(contentType[len(cloudeventsContentType)+1:])
	default:
		return "", false
	}
}

// trimMediaTypeParams cuts RFC 2045 parameters (";" onwards) off a subtype, trims
// the whitespace the grammar allows around the delimiter, and verifies what remains
// can actually be a subtype.
//
// The token check is not decoration. Whatever comes back is handed to
// encoding.GetCodec as a codec name, so anything that is not a subtype must be
// reported as "no subtype" rather than as a name that happens not to resolve —
// otherwise a caller cannot distinguish "unsupported codec" from "malformed content
// type". Checking the grammar rejects that whole class at once; guarding individual
// characters does not (a ';'-and-'='-only guard still accepted "application/=0",
// found by FuzzContentSubtype).
func trimMediaTypeParams(subtype string) (string, bool) {
	subtype, _, _ = strings.Cut(subtype, ";")
	subtype = strings.TrimSpace(subtype)
	if !isMediaTypeToken(subtype) {
		return "", false
	}

	return subtype, true
}

// isMediaTypeToken reports whether s is a non-empty RFC 2045 token — the grammar a
// media subtype must follow: any CHAR except SPACE, CTLs, and tspecials
// ("()<>@,;:\\\"/[]?=").
func isMediaTypeToken(s string) bool {
	if s == "" {
		return false
	}

	for _, r := range s {
		if r <= ' ' || r >= 0x7f || strings.ContainsRune("()<>@,;:\\\"/[]?=", r) {
			return false
		}
	}

	return true
}

// SplitType splits a fully-qualified event type into its service and event parts
// at the last dot: "books.v1.BookCreated" → ("books.v1", "BookCreated").
//
// This is the ONE definition of the event-type shape, used by the publisher (to
// reject a malformed type before it can be persisted), by the subscriber (to route
// an incoming delivery), and by the RabbitMQ sender (to derive exchange and routing
// key). It exists as one function because the shape used to be re-derived at each
// of those points with a raw strings.LastIndex and no -1 guard, which turned a
// dot-less type into a panic in two of them.
//
// The type may come from an incoming message or a persisted outbox row, so a
// malformed value is always an error and never a panic.
func SplitType(eventType string) (service, name string, err error) {
	pos := strings.LastIndex(eventType, ".")
	if pos <= 0 || pos == len(eventType)-1 {
		return "", "", fmt.Errorf("malformed event type %q: want <service>.<event>", eventType)
	}

	return eventType[:pos], eventType[pos+1:], nil
}

// ContentType is ContentSubtype's inverse for the plain application/ family:
// ContentType("proto") == "application/proto".
func ContentType(contentSubtype string) string {
	return baseContentType + "/" + contentSubtype
}
