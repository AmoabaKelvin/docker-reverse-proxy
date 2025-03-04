package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

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
	logger *log.Logger
}

var logger *log.Logger

type RoutingTable struct {
	Routes map[string]*TraefikConfig // map the domain to the details of the container. This will be used to match the request to the correct container
	rw     sync.RWMutex
	logger *log.Logger
}

var routingTable = RoutingTable{
	Routes: make(map[string]*TraefikConfig),
}

func init() {
	logFile, err := os.OpenFile("proxy.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Fatal("Failed to open log file:", err)
	}
	logger = log.New(logFile, "", log.LstdFlags)

	// Initialize the logger field in the routingTable variable after the global logger is initialized
	routingTable.logger = logger
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

	// proxy server
	proxy := http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		// get the domain information from the routing table
		formattedHost := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(req.Host, "http://"), "https://"), ":80")
		domain := routingTable.getRouteForDomain(formattedHost)
		if domain == nil {
			logger.Printf("No route found for domain: %s", formattedHost)
			rw.WriteHeader(http.StatusNotFound)
			rw.Write([]byte("No route found for domain: " + formattedHost))
			return
		}

		address, _, err := net.SplitHostPort(req.RemoteAddr)
		if err != nil {
			logger.Printf("There was an error splitting the host: %s", err)
		}
		req.Header.Set("X-Forwarded-For", address)
		req.URL.Host = domain.IPAddress + ":" + domain.Port
		req.URL.Scheme = "http"
		req.RequestURI = ""
		// after updating the details of the incoming request to now point to
		// the sample server we have, we have to make sure that we now perform
		// the request to that particular server and get the response and write it
		// back to the client

		// we don't want the whole process to block forever if the backend is rather
		// not responding or slow, we need to set a timeout for the request.
		// we have a 30 second timeout for the whole process.
		logger.Printf("Making request to: %s", req.URL.String())
		ctx, cancel := context.WithTimeout(req.Context(), 30*time.Second)
		defer cancel()
		req = req.WithContext(ctx)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				rw.WriteHeader(http.StatusGatewayTimeout)
				rw.Write([]byte("The request timed out"))
			} else {
				rw.WriteHeader(http.StatusInternalServerError)
				rw.Write([]byte("There was an error processing the request"))
			}
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
		logger: logger,
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
	c.logger.Println("Listening to container events...")

	eventsChan, errChan := c.client.Events(context.Background(), events.ListOptions{
		Filters: filters.NewArgs(filters.KeyValuePair{Key: "type", Value: "container"}),
	})

	go func() {
		for {
			select {
			case event := <-eventsChan:
				c.logger.Printf("Received container event: %s", event.Status)
				c.processContainerEvent(event)
			case err := <-errChan:
				if err != nil {
					panic(err)
				}
			}
		}
	}()
}

func (c *Client) inspectAndReturnLabels(containerID string) (map[string]string, error) {
	container, err := c.client.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return nil, err
	}
	return container.Config.Labels, nil
}

// processContainerEvent processes a container event and updates the routing table
// based on the event type.
func (c *Client) processContainerEvent(event events.Message) {
	c.logger.Printf("Processing container event: %s", event.Status)

	// Handle container stop events
	if event.Status == "die" || event.Status == "stop" || event.Status == "kill" {
		c.handleContainerStopEvent(event)
		return
	}

	// Handle only start and update events for the rest of the function
	if event.Status != "start" && event.Status != "update" {
		return
	}

	// Fetch complete container details to get all labels
	container, err := c.client.ContainerInspect(context.Background(), event.Actor.ID)
	if err != nil {
		c.logger.Printf("Error inspecting container %s: %v\n", event.Actor.ID, err)
		return
	}

	// Check if container should be managed by traefik
	if enabled, exists := container.Config.Labels["traefik.enable"]; !exists || enabled != "true" {
		c.logger.Printf("Container %s is not enabled for traefik\n", container.Name)
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

	c.logger.Printf("Traefik Configuration for %s:\n", containerName)
	c.logger.Printf("  Container ID: %s\n", config.ContainerID)
	c.logger.Printf("  Router Name: %s\n", routerName)
	c.logger.Printf("  Rule: %s\n", config.Rule)
	c.logger.Printf("  Port: %s\n", config.Port)
	c.logger.Printf("  IP Address: %s\n", config.IPAddress)
	c.logger.Printf("  Networks: %v\n", config.Networks)
	c.logger.Printf("  All Labels: %v\n", config.Labels)
	c.logger.Printf("  Domain: %s\n", config.Domain)

	// Update routing table
	routingTable.updateRoutingTable(&config)
}

// handleContainerStopEvent handles die, stop, and kill events
func (c *Client) handleContainerStopEvent(event events.Message) {
	containerName := event.Actor.Attributes["name"]
	c.logger.Printf("Container %s is being stopped/killed", containerName)

	// First try to find the container in the routing table by ID
	for domain, config := range routingTable.Routes {
		if config.ContainerID == event.Actor.ID {
			c.logger.Printf("Removing route from routing table for domain: %s\n", domain)
			routingTable.removeRouteFromRoutingTable(domain)
			return
		}
	}

	// If not found in routing table, check if traefik.enable is in the attributes
	if enabled, exists := event.Actor.Attributes["traefik.enable"]; exists && enabled == "true" {
		c.logger.Printf("Container %s has traefik.enable=true but no matching route found\n", containerName)
		return
	}

	// As a last resort, try to inspect the container
	labels, err := c.inspectAndReturnLabels(event.Actor.ID)
	if err != nil {
		c.logger.Printf("Error inspecting container %s: %v\n", event.Actor.ID, err)
		return
	}

	// Check if container should be managed by traefik
	if enabled, exists := labels["traefik.enable"]; !exists || enabled != "true" {
		c.logger.Printf("Container %s is not enabled for traefik\n", event.Actor.ID)
		return
	}

	// Get domain from labels
	domain := getCleanDomainFromHostLabel(labels[fmt.Sprintf("traefik.http.routers.%s.rule", containerName)])
	if domain == "" {
		c.logger.Printf("No domain found for container %s\n", event.Actor.ID)
		return
	}

	c.logger.Printf("Removing route from routing table for domain: %s\n", domain)
	routingTable.removeRouteFromRoutingTable(domain)
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
func (router *RoutingTable) updateRoutingTable(config *TraefikConfig) {
	// Implement your routing table update logic here
	// This could be updating a shared map, database, or other storage
	router.logger.Printf("Updating routing table for domain: %s", config.Domain)

	router.rw.Lock()
	defer router.rw.Unlock()

	router.Routes[config.Domain] = config
	router.logger.Printf("Routing table updated for domain: %s", config.Domain)
	router.logger.Printf("Routing table: %v", router.Routes)
}

// getRouteForDomain gets the routing information for a given domain from the routing table
func (router *RoutingTable) getRouteForDomain(domain string) *TraefikConfig {
	router.rw.RLock()
	defer router.rw.RUnlock()
	return router.Routes[domain]
}

// removeRouteFromRoutingTable removes a route from the routing table
func (router *RoutingTable) removeRouteFromRoutingTable(domain string) {
	router.rw.Lock()
	defer router.rw.Unlock()
	delete(router.Routes, domain)
	router.logger.Printf("Route removed from routing table for domain: %s", domain)
}
