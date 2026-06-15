// Package bus is the service's event-bus seam. .flowmap.yaml names Publish under
// classify.busPublish, so a call to it is an outbound published event: named when
// the topic is a constant, "<dynamic>" when it is a runtime value.
package bus

// Publish emits topic onto the bus.
func Publish(topic string) {}
