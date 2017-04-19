package agent

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"time"

	"github.com/chrislusf/gleam/distributed/resource"
	"github.com/chrislusf/gleam/pb"
	"github.com/golang/protobuf/proto"
	"github.com/kardianos/osext"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

func (as *AgentServer) serveGrpc(listener net.Listener) {
	grpcServer := grpc.NewServer()
	pb.RegisterGleamAgentServer(grpcServer, as)
	grpcServer.Serve(listener)
}

func (as *AgentServer) SendFileResource(stream pb.GleamAgent_SendFileResourceServer) error {
	as.receiveFileResourceLock.Lock()
	defer as.receiveFileResourceLock.Unlock()

	request, err := stream.Recv()
	if err != nil {
		return err
	}

	dir := path.Join(*as.Option.Dir, fmt.Sprintf("%d", request.GetFlowHashCode()), request.GetDir())
	os.MkdirAll(dir, 0755)

	toFile := filepath.Join(dir, request.GetName())
	hasSameHash := false
	if toFileHash, err := resource.GenerateFileHash(toFile); err == nil {
		hasSameHash = toFileHash.Hash == request.GetHash()
	}

	if err := stream.Send(&pb.FileResourceResponse{hasSameHash, true}); err != nil {
		return err
	}

	if hasSameHash {
		return nil
	}

	f, err := os.OpenFile(toFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		request, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		_, err = f.Write(request.GetContent())
		if err != nil {
			log.Printf("Write file error: ", err)
			return err
		}
	}
}

// Execute executes a request and stream stdout and stderr back
func (as *AgentServer) Execute(request *pb.ExecutionRequest, stream pb.GleamAgent_ExecuteServer) error {

	dir := path.Join(*as.Option.Dir, fmt.Sprintf("%d", request.GetInstructions().GetFlowHashCode()), request.GetDir())
	os.MkdirAll(dir, 0755)

	allocated := *request.GetResource()

	as.plusAllocated(allocated)
	defer as.minusAllocated(allocated)

	return as.executeCommand(stream, request, dir)

}

func (as *AgentServer) executeCommand(
	stream pb.GleamAgent_ExecuteServer,
	startRequest *pb.ExecutionRequest,
	dir string,
) (err error) {

	ctx := stream.Context()
	errChan := make(chan error, 3) // normal exit, stdout, stderr
	stopChan := make(chan bool)

	// start the command
	executableFullFilename, _ := osext.Executable()
	command := exec.Command(
		executableFullFilename,
		"execute",
		"--note",
		startRequest.GetName(),
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		log.Printf("Failed to create stdin pipe: %v", err)
		return
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		log.Printf("Failed to create stdout pipe: %v", err)
		return
	}
	stderr, err := command.StderrPipe()
	if err != nil {
		log.Printf("Failed to create stderr pipe: %v", err)
		return
	}
	// msg.Env = startRequest.Envs
	command.Dir = dir

	if err = command.Start(); err != nil {
		log.Printf("Failed to start command %s under %s: %v",
			command.Path, command.Dir, err)
		return err
	}

	go streamOutput(errChan, stream, stdout)
	go streamError(errChan, stream, stderr)
	go streamPulse(errChan, stopChan, stream)
	defer func() { stopChan <- true }()

	// send instruction set to executor
	msgMessageBytes, err := proto.Marshal(startRequest.GetInstructions())
	if err != nil {
		log.Printf("Failed to marshal command %s: %v",
			startRequest.GetInstructions().String(), err)
		return err
	}
	if _, err = stdin.Write(msgMessageBytes); err != nil {
		log.Printf("Failed to write command: %v", err)
		return err
	}
	if err = stdin.Close(); err != nil {
		log.Printf("Failed to close command: %v", err)
		return err
	}

	// wait for finish
	go func() {
		waitErr := command.Wait()
		if waitErr != nil {
			log.Printf("Failed to run command %s: %v", startRequest.GetName(), waitErr)
		}
		// only the command send a nil to errChan
		errChan <- waitErr
	}()

	select {
	case err = <-errChan:
		if err != nil {
			log.Printf("Error running command %s %+v: %v", command.Path, command.Args, err)
			return err
		}
		return sendExitStats(stream, command)
	case <-ctx.Done():
		log.Printf("Cancelled command %s %+v", command.Path, command.Args)
		if err := command.Process.Kill(); err != nil {
			log.Printf("failed to kill: %v", err)
		}
		if err := command.Process.Release(); err != nil {
			log.Printf("failed to release: %v", err)
		}
		return ctx.Err()
	}

}

// Delete deletes a particular dataset shard
func (as *AgentServer) Delete(ctx context.Context, deleteRequest *pb.DeleteDatasetShardRequest) (*pb.DeleteDatasetShardResponse, error) {

	log.Println("deleting", deleteRequest.Name)
	as.storageBackend.DeleteNamedDatasetShard(deleteRequest.Name)
	as.inMemoryChannels.Cleanup(deleteRequest.Name)

	return &pb.DeleteDatasetShardResponse{}, nil
}

func streamOutput(errChan chan error, stream pb.GleamAgent_ExecuteServer, reader io.Reader) {

	buffer := make([]byte, 1024)
	for {
		n, err := reader.Read(buffer)
		if err == io.EOF {
			return
		}
		if err != nil {
			errChan <- fmt.Errorf("Failed to read stdout: %v", err)
			return
		}
		if n == 0 {
			continue
		}

		if sendErr := stream.Send(&pb.ExecutionResponse{
			Output: buffer[0:n],
		}); sendErr != nil {
			errChan <- fmt.Errorf("Failed to send stdout: %v", sendErr)
			return
		}
	}
}

func streamError(errChan chan error, stream pb.GleamAgent_ExecuteServer, reader io.Reader) {

	tee := io.TeeReader(reader, os.Stderr)

	buffer := make([]byte, 1024)
	for {
		n, err := tee.Read(buffer)
		if err == io.EOF {
			break
		}
		if err != nil {
			errChan <- fmt.Errorf("Failed to read stderr: %v", err)
			return
		}
		if n == 0 {
			continue
		}

		if sendErr := stream.Send(&pb.ExecutionResponse{
			Error: buffer[0:n],
		}); sendErr != nil {
			errChan <- fmt.Errorf("Failed to send stderr: %v", sendErr)
			return
		}
	}
}

func streamPulse(errChan chan error, stopChan chan bool, stream pb.GleamAgent_ExecuteServer) error {

	tickChan := time.NewTicker(time.Minute).C
	for {
		select {
		case <-stopChan:
			return nil
		case <-tickChan:
			if sendErr := stream.Send(&pb.ExecutionResponse{}); sendErr != nil {
				return fmt.Errorf("Failed to send empty response: %v\n", sendErr)
			}
		}
	}
}

func sendExitStats(stream pb.GleamAgent_ExecuteServer, cmd *exec.Cmd) error {
	if cmd.ProcessState != nil {
		if sendErr := stream.Send(&pb.ExecutionResponse{
			SystemTime: cmd.ProcessState.SystemTime().Seconds(),
			UserTime:   cmd.ProcessState.UserTime().Seconds(),
		}); sendErr != nil {
			return fmt.Errorf("Failed to send exit stats response: %v\n", sendErr)
		}
	}
	return nil
}

func (as *AgentServer) plusAllocated(allocated pb.ComputeResource) {
	as.allocatedResourceLock.Lock()
	defer as.allocatedResourceLock.Unlock()
	*as.allocatedResource = as.allocatedResource.Plus(allocated)
}

func (as *AgentServer) minusAllocated(allocated pb.ComputeResource) {
	as.allocatedResourceLock.Lock()
	defer as.allocatedResourceLock.Unlock()
	*as.allocatedResource = as.allocatedResource.Minus(allocated)
}
