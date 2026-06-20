// Package bus is the service's event-bus seam. .flowmap.yaml names Publish under
// classify.busPublish, so a call to it is an outbound published event.
package bus

// Publish emits topic onto the bus.
func Publish(topic string) {}
