package api

import (
	"bytes"
	"fmt"
	"io"
	"syscall"

	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"github.com/ernoaapa/can/pkg/api/mapping"
	containers "github.com/ernoaapa/can/pkg/api/services/containers/v1"
	pods "github.com/ernoaapa/can/pkg/api/services/pods/v1"
	"github.com/ernoaapa/can/pkg/progress"
)

// Client connects to RPC server
type Client struct {
	namespace  string
	serverAddr string
	ctx        context.Context
}

// AttachIO wraps stdin/stdout for attach
type AttachIO struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

func NewAttachIO(stdin io.Reader, stdout, stderr io.Writer) AttachIO {
	return AttachIO{stdin, stdout, stderr}
}

// NewClient creates new RPC server client
func NewClient(namespace, serverAddr string) *Client {
	return &Client{
		namespace,
		serverAddr,
		context.Background(),
	}
}

// GetPods calls server and fetches all pods information
func (c *Client) GetPods() ([]*pods.Pod, error) {
	conn, err := grpc.Dial(c.serverAddr, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := pods.NewPodsClient(conn)
	resp, err := client.List(c.ctx, &pods.ListPodsRequest{
		Namespace: c.namespace,
	})
	if err != nil {
		return nil, err
	}

	return resp.GetPods(), nil
}

// GetPod return Pod by name
func (c *Client) GetPod(podName string) (*pods.Pod, error) {
	pods, err := c.GetPods()
	if err != nil {
		return nil, err
	}

	for _, pod := range pods {
		if pod.Metadata.Name == podName {
			return pod, nil
		}
	}
	return nil, fmt.Errorf("No pod found with name [%s]", podName)
}

// PodOpts adds more information to the Pod going to be created
type PodOpts func(pod *pods.Pod) error

// CreatePod creates new pod to the target server
func (c *Client) CreatePod(pod *pods.Pod, opts ...PodOpts) error {
	for _, o := range opts {
		err := o(pod)
		if err != nil {
			return err
		}
	}

	conn, err := grpc.Dial(c.serverAddr, grpc.WithInsecure())
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pods.NewPodsClient(conn)
	stream, err := client.Create(c.ctx, &pods.CreatePodRequest{
		Pod: pod,
	})
	if err != nil {
		return err
	}

	progress := progress.NewRenderer()
	defer progress.Stop()

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			err = stream.CloseSend()
			progress.Done()
			return err
		}
		if err != nil {
			return err
		}

		progress.Update(mapping.MapAPIModelToImageFetchProgress(resp.Images))
	}
}

// StartPod creates new pod to the target server
func (c *Client) StartPod(name string) (*pods.Pod, error) {
	conn, err := grpc.Dial(c.serverAddr, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := pods.NewPodsClient(conn)
	resp, err := client.Start(c.ctx, &pods.StartPodRequest{
		Namespace: c.namespace,
		Name:      name,
	})
	if err != nil {
		return nil, err
	}

	return resp.GetPod(), nil
}

// DeletePod creates new pod to the target server
func (c *Client) DeletePod(pod *pods.Pod) (*pods.Pod, error) {
	conn, err := grpc.Dial(c.serverAddr, grpc.WithInsecure())
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	client := pods.NewPodsClient(conn)

	resp, err := client.Delete(c.ctx, &pods.DeletePodRequest{
		Namespace: pod.Metadata.Namespace,
		Name:      pod.Metadata.Name,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetPod(), nil
}

// Attach calls server and fetches pod logs
func (c *Client) Attach(containerID string, attachIO AttachIO) (err error) {
	done := make(chan struct{})
	errc := make(chan error)

	md := metadata.Pairs(
		"namespace", c.namespace,
		"container", containerID,
	)
	ctx := metadata.NewOutgoingContext(c.ctx, md)
	conn, err := grpc.Dial(c.serverAddr, grpc.WithInsecure())
	if err != nil {
		return err
	}
	defer conn.Close()

	client := containers.NewContainersClient(conn)
	log.Debugf("Open stream connection to server to get logs")
	stream, err := client.Attach(ctx)
	if err != nil {
		return err
	}

	go func() {
		defer close(done)
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				err = stream.CloseSend()
				if err != nil {
					errc <- err
				}
				break
			}
			if err != nil {
				errc <- errors.Wrapf(err, "Received error while reading attach stream")
				break
			}

			target := attachIO.Stdout
			if resp.Stderr {
				target = attachIO.Stderr
			}

			_, err = io.Copy(target, bytes.NewReader(resp.Output))
			if err != nil {
				errc <- errors.Wrapf(err, "Error while copying data")
				break
			}
		}
	}()

	if attachIO.Stdin != nil {
		go func() {
			defer close(done)

			for {
				buf := make([]byte, 1024)
				n, err := attachIO.Stdin.Read(buf)
				if err == io.EOF {
					// nothing else to pipe, kill this goroutine
					break
				}
				if err != nil {
					errc <- errors.Wrapf(err, "Error while reading stdin to buffer")
					break
				}

				err = stream.Send(&containers.StdinStreamRequest{
					Input: buf[:n],
				})
				if err != nil {
					errc <- errors.Wrapf(err, "Sending to stream returned error")
					break
				}
			}
		}()
	}

	for {
		select {
		case <-done:
			return err
		case err = <-errc:
		}
	}
}

// Signal sends kill signal to container process
func (c *Client) Signal(containerID string, signal syscall.Signal) (err error) {
	conn, err := grpc.Dial(c.serverAddr, grpc.WithInsecure())
	if err != nil {
		return err
	}
	defer conn.Close()

	client := containers.NewContainersClient(conn)

	_, err = client.Signal(c.ctx, &containers.SignalRequest{
		Namespace:   c.namespace,
		ContainerID: containerID,
		Signal:      int32(signal),
	})

	return err
}
