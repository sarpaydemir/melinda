// Package gateway keeps the chat platform's intake session open, reconnecting with backoff, and passes inbound
// events to the dispatcher's Enqueue, which serves as the session's chat.Deliver.
package gateway
