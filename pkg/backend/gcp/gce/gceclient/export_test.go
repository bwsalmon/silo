package gceclient

// ComputeEndpointForTest is the compute/v1 base URL c was made with.
func (c *Client) ComputeEndpointForTest() string { return c.svc.BasePath }
