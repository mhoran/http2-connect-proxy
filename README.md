# http2-connect-proxy
http2-connect-proxy can be used to tunnel arbitrary TCP traffic over HTTP/2. It was originally designed to tunnel MySQL traffic over HTTP/2, but can be used to tunnel any TCP protocol. It has been tested with the [Envoy proxy](https://github.com/envoyproxy/envoy) terminating the HTTP/2 connection, but should work with any HTTP/2 server that supports the CONNECT method.
