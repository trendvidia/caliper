package testpkg

type Client struct{}
type Server struct{}

func (c *Client) Do(args ...any) {}
func (s *Server) Do(args ...any) {}

var ctx any

func process() {
	client := &Client{}
	server := &Server{}
	var req, payload any
	client.Do(ctx, req)
	server.Do(ctx, payload)
}
