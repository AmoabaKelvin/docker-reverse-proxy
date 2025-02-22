package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// writing a reverse proxy for the minimal traefik project.
// we will need to spin up a server and then the main things we would want
// the reverse proxy to do will be to forward requests to the server, copy
// over the client's information and then make the request on behalf of the
// client. Once the request goes through and we receive a response, process
// the response, copy the response headers and add it to the response that will
// be sent to the client. we will need to ensure we copy all the response headers
// and send them back to the client.
// this is going to be for a bigger project. building a minimal traefik

type Client struct {
	client *client.Client
}

func main() {
	// a reverse proxy must have an actual url to forward the requests to
	// at the moment, this will be a hardcoded http server that we spin up
	// locally, but when things are being taken to the level of say minimal
	// traefik, we would have to parse the incoming request, figure out the
	// parts of most interest and check that against the routing table that
	// will be in place. The routing table will let us know the details about
	// the container that will be serving up the request.
	// we might have something like this |domain|container/service|port|

	u, err := url.Parse("http://127.0.0.1:8000")
	if err != nil {
		fmt.Println(err)
	}

	// proxy server
	proxy := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		// we are going to use the parsed url as the demo url we wanna play with
		// and be forwarding requests to and receiving responses from.

		address, _, err := net.SplitHostPort(req.RemoteAddr)
		if err != nil {
			fmt.Println("There was an error splitting the host")
		}
		req.Header.Set("X-Forwarded-For", address)
		req.URL.Host = u.Host
		req.URL.Scheme = u.Scheme
		req.RequestURI = ""

		// after updating the details of the incoming request to now point to
		// the sample server we have, we have to make sure that we now perform
		// the request to that particular server and get the response and write it
		// back to the client
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			rw.WriteHeader(http.StatusInternalServerError)
			rw.Write([]byte("There was an error processing the request"))
			return
		}
		defer response.Body.Close()

		// we have to copy the response headers from the actual server
		// and write them to the response for the client. else, we might miss
		// out on some important headers that the client might need to process.
		// like attaching wrong content type to the response.
		// we also have to write the status code of the response to the client
		for key, values := range response.Header {
			for _, value := range values {
				rw.Header().Add(key, value)
			}
		}

		// we need to handle cases of streaming responses and be sending
		// the response back to the client as it comes in.
		if isStreaming(response) {
			// we need to get the underlying connection to the client so
			// that we can write to it as the response comes in
			// we can use the hijacker interface to do this
			hijacker, ok := rw.(http.Hijacker)
			if !ok {
				http.Error(rw, "Streaming unsupported", http.StatusInternalServerError)
				return
			}
			conn, bufrw, err := hijacker.Hijack()
			if err != nil {
				http.Error(rw, "Streaming unsupported", http.StatusInternalServerError)
				return
			}
			defer conn.Close()

			fmt.Fprintf(bufrw, "HTTP/1.1 %d %s\r\n", response.StatusCode, http.StatusText(response.StatusCode))
			response.Header.Write(bufrw)
			fmt.Fprintf(bufrw, "\r\n")
			bufrw.Flush()

			done := make(chan bool) // channel to signal when the streaming is done

			// let's start a goroutine to handle the streaming of the data to the client
			go func() {
				buffer := make([]byte, 32*1024)
				for {
					// Read from response body
					n, err := response.Body.Read(buffer)
					if n > 0 {
						_, writeErr := bufrw.Write(buffer[:n])
						if writeErr != nil {
							fmt.Printf("Error writing to client: %v\n", writeErr)
							break
						}
						bufrw.Flush()
					}
					if err != nil {
						if err != io.EOF {
							fmt.Printf("Error reading from response: %v\n", err)
						}
						break
					}
				}
				done <- true
			}()

			// Wait for either completion or connection close.
			// we send in a done channel to signal when the streaming is done
			select {
			case <-done:
				fmt.Println("Streaming completed")
			case <-req.Context().Done():
				fmt.Println("Client disconnected")
			}
		} else {
			rw.WriteHeader(response.StatusCode)
			io.Copy(rw, response.Body)
		}
	})
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		panic(err)
	}
	dockerClient := Client{
		client: cli,
	}
	go dockerClient.listenToContainerEvents()
	http.ListenAndServe(":8080", proxy)
}

func isStreaming(response *http.Response) bool {
	return response.Header.Get("Content-Type") == "text/event-stream" ||
		response.Header.Get("Transfer-Encoding") == "chunked" ||
		response.Header.Get("Connection") == "keep-alive"
}

func (c *Client) listenToContainerEvents() {
	// we are to listen to events from docker here and process events about
	// a container, such as a new container being created, a container being
	// started, a container being stopped, a container being removed, etc.
	// we will then update the routing table with the details of the container
	// and the details of the service that will be serving up requests for
	// that container.
	fmt.Println("Listening to container events...")

	eventsChan, errChan := c.client.Events(context.Background(), events.ListOptions{
		Filters: filters.NewArgs(filters.KeyValuePair{Key: "type", Value: "container"}),
	})

	go func() {
		for {
			select {
			case event := <-eventsChan:
				c.processContainerEvent(event)
			case err := <-errChan:
				if err != nil {
					panic(err)
				}
			}
		}
	}()
}

// processContainerEvent processes a container event and updates the routing table
// based on the event type.
func (c *Client) processContainerEvent(event events.Message) {
	switch event.Status {
	case "start", "update":
		// Continue processing
	default:
		return // Ignore other events
	}

	container, err := c.client.ContainerInspect(context.Background(), event.Actor.ID)
	if err != nil {
		fmt.Printf("Error inspecting container %s: %v\n", event.Actor.ID, err)
		return
	}

	// Fetch complete container details to get all labels
	container, err = c.client.ContainerInspect(context.Background(), event.Actor.ID)
	if err != nil {
		fmt.Printf("Error inspecting container %s: %v\n", event.Actor.ID, err)
		return
	}

	// Check if container should be managed by traefik. If not, we don't need to process it
	// we will need to check the routing table to see if this container is being used in any of the routes
	// and if so, we need to remove it from the routing table.
	// todo: implement this
	if enabled, exists := container.Config.Labels["traefik.enable"]; !exists || enabled != "true" {
		fmt.Printf("Container %s is not enabled for traefik\n", container.Name)
		return
	}

	containerName := strings.TrimPrefix(container.Name, "/") // Remove leading slash
	labels := container.Config.Labels

	config := TraefikConfig{
		ContainerID:   container.ID,
		ContainerName: containerName,
		Labels:        make(map[string]string),
		Networks:      make(map[string]string),
	}

	// Extract network information from the container
	for networkName, network := range container.NetworkSettings.Networks {
		config.Networks[networkName] = network.IPAddress
		// Use the first network's IP as the default IPAddress
		if config.IPAddress == "" {
			config.IPAddress = network.IPAddress
		}
	}

	// Extract all traefik-related labels from the container
	for key, value := range labels {
		if strings.HasPrefix(key, "traefik.") {
			config.Labels[key] = value
		}
	}

	// Get the main routing configuration specified in the labels
	routerName := getRouterName(labels, containerName)
	config.Rule = labels[fmt.Sprintf("traefik.http.routers.%s.rule", routerName)]
	config.Domain = getCleanDomainFromHostLabel(config.Rule)
	config.Port = labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", routerName)]

	fmt.Printf("Traefik Configuration for %s:\n", containerName)
	fmt.Printf("  Container ID: %s\n", config.ContainerID)
	fmt.Printf("  Router Name: %s\n", routerName)
	fmt.Printf("  Rule: %s\n", config.Rule)
	fmt.Printf("  Port: %s\n", config.Port)
	fmt.Printf("  IP Address: %s\n", config.IPAddress)
	fmt.Printf("  Networks: %v\n", config.Networks)
	fmt.Printf("  All Labels: %v\n", config.Labels)
	fmt.Printf("  Domain: %s\n", config.Domain)
	// TODO: Update your routing table here
	updateRoutingTable(config)
}

// TraefikConfig holds the parsed configuration for a container
type TraefikConfig struct {
	ContainerID   string
	ContainerName string
	Domain        string
	Rule          string
	Port          string
	IPAddress     string
	Networks      map[string]string
	Labels        map[string]string
}

// getRouterName tries to determine the router name from labels or falls back to container name
func getRouterName(labels map[string]string, containerName string) string {
	for key := range labels {
		if strings.HasPrefix(key, "traefik.http.routers.") && strings.HasSuffix(key, ".rule") {
			parts := strings.Split(key, ".")
			if len(parts) > 3 {
				return parts[3]
			}
		}
	}
	// Fall back to container name
	return containerName
}

// getCleanDomainFromHostLabel should get the actual domain in the Host rule
// for example, if the host rule is "Host(`example.com`)", the domain should be
// "example.com"
func getCleanDomainFromHostLabel(label string) string {
	// we need to parse the label and get the domain from the Host rule
	// the label will be in the format of "Host(`example.com`)", so we need to
	// extract the domain from the Host rule
	parts := strings.Split(label, "`")
	if len(parts) > 1 {
		return parts[1]
	}
	return ""
}

// updateRoutingTable updates the routing table with the details of the container
// this will be a map of the router name to the details of the container
// that is serving up requests for that router.
func updateRoutingTable(config TraefikConfig) {
	// Implement your routing table update logic here
	// This could be updating a shared map, database, or other storage
}
