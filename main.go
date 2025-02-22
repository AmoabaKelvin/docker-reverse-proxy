package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

		rw.WriteHeader(response.StatusCode)
		io.Copy(rw, response.Body)
	})
	http.ListenAndServe(":8080", proxy)
}
