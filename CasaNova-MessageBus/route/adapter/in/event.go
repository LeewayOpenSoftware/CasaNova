package in

import (
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/codegen"
	"github.com/LeewayOpenSoftware/CasaNova-MessageBus/model"
)

func EventAdapter(event codegen.Event) model.Event {
	// properties := make([]model.Property, 0)
	// for _, property := range  {
	// 	properties = append(properties, PropertyAdapter(property))
	// }

	var timestamp int64
	if event.Timestamp != nil {
		timestamp = event.Timestamp.Unix()
	}

	var uuid string
	if event.Uuid != nil {
		uuid = *event.Uuid
	}

	return model.Event{
		SourceID:   event.SourceID,
		Name:       event.Name,
		Properties: event.Properties,
		UUID:       uuid,

		Timestamp: timestamp,
	}
}
