package instruction

import (
	"io"
	"log"

	"github.com/chrislusf/gleam/pb"
	"github.com/chrislusf/gleam/util"
)

func init() {
	InstructionRunner.Register(func(m *pb.Instruction) Instruction {
		if m.GetMergeTo() != nil {
			return NewMergeTo()
		}
		return nil
	})
}

type MergeTo struct{}

func NewMergeTo() *MergeTo {
	return &MergeTo{}
}

func (b *MergeTo) Name() string {
	return "MergeTo"
}

func (b *MergeTo) Function() func(readers []io.Reader, writers []io.Writer, stats *Stats) error {
	return func(readers []io.Reader, writers []io.Writer, stats *Stats) error {
		return DoMergeTo(readers, writers[0])
	}
}

func (b *MergeTo) SerializeToCommand() *pb.Instruction {
	return &pb.Instruction{
		Name:    b.Name(),
		MergeTo: &pb.Instruction_MergeTo{},
	}
}

func (b *MergeTo) GetMemoryCostInMB(partitionSize int64) int64 {
	return 3
}

// Top streamingly compare and get the top n items
func DoMergeTo(readers []io.Reader, writer io.Writer) error {
	// enqueue one item to the pq from each channel
	for _, reader := range readers {
		x, err := util.ReadMessage(reader)
		for err == nil {
			if err := util.WriteMessage(writer, x); err != nil {
				return err
			}
			x, err = util.ReadMessage(reader)
		}
		if err != io.EOF {
			log.Printf("DoMergeTo failed start :%v", err)
			return err
		}
	}
	return nil
}
