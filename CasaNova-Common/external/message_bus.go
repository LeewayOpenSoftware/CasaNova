package external

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

const (
	MessageBusAddressFilename = "message-bus.url"
	APIMessageBus             = "/v2/message_bus"

	// MessageBusSocketDir/Path: the bus's trusted unix socket for sibling
	// ROOT services. It lives in the shared 0700 internal dir under
	// /run/casanova (every unit's RuntimeDirectory, Preserve=yes) — NOT in
	// /tmp: every unit has PrivateTmp=yes, so a /tmp socket is only visible
	// inside the bus's own private namespace and every consumer dials into
	// the void (the historical "message-bus caído" outage). Centralized here
	// because both the server (CasaNova-MessageBus) and every consumer
	// import this package.
	MessageBusSocketDir  = "/run/casanova/internal"
	MessageBusSocketPath = MessageBusSocketDir + "/message-bus.sock"

	// MessageBusSocketServer is the base URL for codegen clients pointed at
	// the socket transport — the "unix" host is decorative (the DialContext
	// ignores it and dials the socket).
	MessageBusSocketServer = "http://unix" + APIMessageBus
)

var (
	socketClient     *http.Client
	socketClientOnce sync.Once
)

// SocketHTTPClient returns the shared http.Client that dials the message-bus
// unix socket. Requests through it reach the bus's socket listener, whose
// ConnContext stamp makes the JWT middleware treat the caller as a trusted
// internal service — the ONLY authorized path for tokenless services since
// the loopback-trust removal. Pass it to codegen clients via WithHTTPClient
// with MessageBusSocketServer as the base URL.
func SocketHTTPClient() *http.Client {
	socketClientOnce.Do(func() {
		dialer := &net.Dialer{}
		socketClient = &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dialer.DialContext(ctx, "unix", MessageBusSocketPath)
				},
			},
		}
	})
	return socketClient
}

type EventType struct {
	Name             string
	SourceID         string
	PropertyTypeList []PropertyType
}

type PropertyType struct {
	Name        string
	Description *string
	Example     *string
}

func PrintEventTypesAsMarkdown(sourceID, version string, eventTypes []EventType) {
	fmt.Printf("## Source ID: `%s` (v%s)\n\n", sourceID, version)
	for _, eventType := range eventTypes {
		fmt.Printf("### Event Type: `%s`\n\n", eventType.Name)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)

		fmt.Fprintln(w, "| Property\t| Description\t| Example\t|")
		fmt.Fprintln(w, "| --------\t| -----------\t| -------\t|")

		for _, propertyType := range eventType.PropertyTypeList {
			fmt.Fprintf(w, "| `%s`\t|", propertyType.Name)

			if propertyType.Description != nil {
				fmt.Fprintf(w, " %s", *propertyType.Description)
			}

			fmt.Fprintf(w, "\t|")

			if propertyType.Example != nil {
				fmt.Fprintf(w, " `%s`", *propertyType.Example)
			}

			fmt.Fprintln(w, "\t|")
		}
		w.Flush()

		fmt.Println()
	}
}

func GetMessageBusAddress(runtimePath string) (string, error) {
	address, err := getAddress(filepath.Join(runtimePath, MessageBusAddressFilename))
	if err != nil {
		return "", err
	}

	return strings.TrimRight(address, "/") + APIMessageBus, nil
}

func PublishEventInSocket(ctx context.Context, SourceID string, Name string, properties map[string]string) (*http.Response, error) {
	httpClient := SocketHTTPClient()

	body, err := json.Marshal(properties)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("http://unix/v2/message_bus/event/%s/%s", SourceID, Name),
		bytes.NewBuffer(body),
	)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return resp, nil
}
