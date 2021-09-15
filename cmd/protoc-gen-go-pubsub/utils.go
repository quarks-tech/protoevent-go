package main

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
)

const messageSuffix = "EventData"

func protocVersion(gen *protogen.Plugin) string {
	v := gen.Request.GetCompilerVersion()
	if v == nil {
		return "(unknown)"
	}
	var suffix string
	if s := v.GetSuffix(); s != "" {
		suffix = "-" + s
	}
	return fmt.Sprintf("%d.%d.%d%s", v.GetMajor(), v.GetMinor(), v.GetPatch(), suffix)
}

func filterEventDataMessages(messages []*protogen.Message) []*protogen.Message {
	var result []*protogen.Message

	for _, m := range messages {
		if strings.HasSuffix(m.GoIdent.String(), messageSuffix) {
			result = append(result, m)
		}
	}

	return result
}

func removeMessageSuffix(m string) string {
	return strings.Replace(m, messageSuffix, "", 1)
}

func unexport(s string) string {
	return strings.ToLower(s[:1]) + s[1:]
}

func quote(s string) string {
	return fmt.Sprintf(`"%s"`, s)
}
