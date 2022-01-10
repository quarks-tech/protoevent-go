package main

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/compiler/protogen"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
)

const messageSuffix = "Event"

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

func filterEventMessages(messages []*protogen.Message, extTypes *protoregistry.Types) ([]*protogen.Message, error) {
	var result []*protogen.Message

	for _, m := range messages {
		options := m.Desc.Options().(*descriptorpb.MessageOptions)
		b, err := proto.Marshal(options)
		if err != nil {
			return nil, err
		}

		options.Reset()

		err = proto.UnmarshalOptions{Resolver: extTypes}.Unmarshal(b, options)
		if err != nil {
			return nil, err
		}

		var isEventMessage bool

		// Use protobuf reflection to iterate over all the extension fields,
		// looking for the ones that we are interested in.
		options.ProtoReflect().Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if !fd.IsExtension() {
				return true
			}

			if fd.FullName() == "quarks_tech.protoevent.v1.enabled" {
				isEventMessage = v.Bool()

				return false
			}

			return true
		})

		if !isEventMessage {
			continue
		}

		if !strings.HasSuffix(m.GoIdent.String(), messageSuffix) {
			return nil, fmt.Errorf("'%s' event must be suffixed with '%s'", m.GoIdent.String(), messageSuffix)
		}

		result = append(result, m)
	}

	return result, nil
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
