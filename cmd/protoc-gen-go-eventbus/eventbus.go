package main

import (
	"google.golang.org/protobuf/compiler/protogen"
)

const (
	contextPackage  = protogen.GoImportPath("context")
	eventPackage    = protogen.GoImportPath("github.com/quarks-tech/protoevent-go/pkg/event")
	eventbusPackage = protogen.GoImportPath("github.com/quarks-tech/protoevent-go/pkg/eventbus")
)

func generateFile(gen *protogen.Plugin, f *protogen.File) {
	filename := f.GeneratedFilenamePrefix + "_eventbus.pb.go"

	g := gen.NewGeneratedFile(filename, f.GoImportPath)
	g.P("// Code generated by protoc-gen-go-eventbus. DO NOT EDIT.")
	g.P("// versions:")
	g.P("// - protoc-gen-go-eventbus v", version)
	g.P("// - protoc                 v", protocVersion(gen))

	if f.Proto.GetOptions().GetDeprecated() {
		g.P("// ", f.Desc.Path(), " is a deprecated file.")
	} else {
		g.P("// source: ", f.Desc.Path())
	}

	g.P()
	g.P("package ", f.GoPackageName)
	g.P()

	generateFileContent(f, g)
}

func generateFileContent(f *protogen.File, g *protogen.GeneratedFile) {
	genPubSub(g, f, filterEventDataMessages(f.Messages))
}

func genPubSub(g *protogen.GeneratedFile, f *protogen.File, messages []*protogen.Message) {
	publisherName := "EventPublisher"

	// Publisher interface
	g.P("type ", publisherName, " interface {")
	for _, m := range messages {
		g.P("Publish", removeMessageSuffix(m.GoIdent.GoName), "Event", "(ctx ", contextPackage.Ident("Context"), ", e *", m.GoIdent.GoName, ", opts ...", eventbusPackage.Ident("PublishOption"), ") error")
	}
	g.P("}")
	g.P()

	// Publisher struct
	g.P("type ", unexport(publisherName), " struct {")
	g.P("pp ", eventbusPackage.Ident("Publisher"))
	g.P("}")
	g.P()

	// Publisher factory
	g.P("func New", publisherName, " (pp ", eventbusPackage.Ident("Publisher"), ") ", publisherName, " {")
	g.P("return &", unexport(publisherName), "{pp}")
	g.P("}")
	g.P()

	// Publisher implementation
	for _, m := range messages {
		eventName := removeMessageSuffix(m.GoIdent.GoName)

		g.P("func (p *", unexport(publisherName), ")", " Publish", eventName, "Event", "(ctx ", contextPackage.Ident("Context"), ", e *", m.GoIdent.GoName, ", opts ...", eventbusPackage.Ident("PublishOption"), ") error {")
		g.P("return p.pp.Publish(ctx, ", quote(removeMessageSuffix(string(m.Desc.FullName()))), ", e, opts...)")
		g.P("}")
		g.P()
	}

	// EventHandler interfaces
	for _, m := range messages {
		eventName := removeMessageSuffix(m.GoIdent.GoName)

		g.P("type ", eventName, "EventHandler", " interface {")
		g.P("Handle", eventName, "Event(ctx ", contextPackage.Ident("Context"), ", e *", m.GoIdent.GoName, ") error")
		g.P("}")
		g.P()
	}

	// EventHandler registration
	for _, m := range messages {
		eventName := removeMessageSuffix(m.GoIdent.GoName)

		g.P("func Register", eventName, "EventHandler(r ", eventbusPackage.Ident("EventHandlerRegistrar"), ", h ", eventName, "EventHandler", ") {")
		g.P("r.RegisterEventHandler(&EventbusServiceDesc,", quote(removeMessageSuffix(string(m.Desc.Name()))), ", h)")
		g.P("}")
		g.P()
	}

	// Handler
	for _, m := range messages {
		eventName := removeMessageSuffix(m.GoIdent.GoName)

		g.P("func _", eventName, "Event_Handler(h interface{}, md *", eventPackage.Ident("Metadata"), ", ctx ", contextPackage.Ident("Context"), ", dec func(interface{}) error, interceptor ", eventbusPackage.Ident("SubscriberInterceptor"), ") error {")
		g.P("e := new(", m.GoIdent.GoName, ")")
		g.P("if err := dec(e); err != nil {")
		g.P("return err")
		g.P("}")
		g.P("if interceptor == nil {")
		g.P("return h.(", eventName, "EventHandler", ").", " Handle", eventName, "Event(ctx, e)")
		g.P("}")
		g.P("handler := func(ctx context.Context, e interface{}) error {")
		g.P("return h.(", eventName, "EventHandler", ").", " Handle", eventName, "Event(ctx, e.(*", m.GoIdent.GoName, "))")
		g.P("}")
		g.P("return interceptor(ctx, md, e, handler)")
		g.P("}")
		g.P()
	}

	// Service description
	g.P("var EventbusServiceDesc = ", eventbusPackage.Ident("ServiceDesc"), "{")
	g.P("ServiceName: ", quote(string(f.Desc.FullName())), ",")
	g.P("Events: []", eventbusPackage.Ident("EventDesc"), "{")

	for _, m := range messages {
		eventName := removeMessageSuffix(m.GoIdent.GoName)
		g.P("{")
		g.P("Name: ", quote(removeMessageSuffix(string(m.Desc.Name()))), ",")
		g.P("HandlerType: (*", eventName, "EventHandler", ")(nil),")
		g.P("Handler: _", eventName, "Event_Handler,")
		g.P("},")
	}
	g.P("},")
	g.P("Metadata: ", quote(f.Desc.Path()), ",")
	g.P("}")
}
