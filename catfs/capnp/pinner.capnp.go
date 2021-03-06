// Code generated by capnpc-go. DO NOT EDIT.

package capnp

import (
	capnp "zombiezen.com/go/capnproto2"
	text "zombiezen.com/go/capnproto2/encoding/text"
	schemas "zombiezen.com/go/capnproto2/schemas"
)

type Pin struct{ capnp.Struct }

// Pin_TypeID is the unique identifier for the type Pin.
const Pin_TypeID = 0x985d53e01674ee95

func NewPin(s *capnp.Segment) (Pin, error) {
	st, err := capnp.NewStruct(s, capnp.ObjectSize{DataSize: 16, PointerCount: 0})
	return Pin{st}, err
}

func NewRootPin(s *capnp.Segment) (Pin, error) {
	st, err := capnp.NewRootStruct(s, capnp.ObjectSize{DataSize: 16, PointerCount: 0})
	return Pin{st}, err
}

func ReadRootPin(msg *capnp.Message) (Pin, error) {
	root, err := msg.RootPtr()
	return Pin{root.Struct()}, err
}

func (s Pin) String() string {
	str, _ := text.Marshal(0x985d53e01674ee95, s.Struct)
	return str
}

func (s Pin) Inode() uint64 {
	return s.Struct.Uint64(0)
}

func (s Pin) SetInode(v uint64) {
	s.Struct.SetUint64(0, v)
}

func (s Pin) IsPinned() bool {
	return s.Struct.Bit(64)
}

func (s Pin) SetIsPinned(v bool) {
	s.Struct.SetBit(64, v)
}

// Pin_List is a list of Pin.
type Pin_List struct{ capnp.List }

// NewPin creates a new list of Pin.
func NewPin_List(s *capnp.Segment, sz int32) (Pin_List, error) {
	l, err := capnp.NewCompositeList(s, capnp.ObjectSize{DataSize: 16, PointerCount: 0}, sz)
	return Pin_List{l}, err
}

func (s Pin_List) At(i int) Pin { return Pin{s.List.Struct(i)} }

func (s Pin_List) Set(i int, v Pin) error { return s.List.SetStruct(i, v.Struct) }

func (s Pin_List) String() string {
	str, _ := text.MarshalList(0x985d53e01674ee95, s.List)
	return str
}

// Pin_Promise is a wrapper for a Pin promised by a client call.
type Pin_Promise struct{ *capnp.Pipeline }

func (p Pin_Promise) Struct() (Pin, error) {
	s, err := p.Pipeline.Struct()
	return Pin{s}, err
}

// A single entry for a certain content node
type PinEntry struct{ capnp.Struct }

// PinEntry_TypeID is the unique identifier for the type PinEntry.
const PinEntry_TypeID = 0xdb74f7cf7bc815c6

func NewPinEntry(s *capnp.Segment) (PinEntry, error) {
	st, err := capnp.NewStruct(s, capnp.ObjectSize{DataSize: 0, PointerCount: 1})
	return PinEntry{st}, err
}

func NewRootPinEntry(s *capnp.Segment) (PinEntry, error) {
	st, err := capnp.NewRootStruct(s, capnp.ObjectSize{DataSize: 0, PointerCount: 1})
	return PinEntry{st}, err
}

func ReadRootPinEntry(msg *capnp.Message) (PinEntry, error) {
	root, err := msg.RootPtr()
	return PinEntry{root.Struct()}, err
}

func (s PinEntry) String() string {
	str, _ := text.Marshal(0xdb74f7cf7bc815c6, s.Struct)
	return str
}

func (s PinEntry) Pins() (Pin_List, error) {
	p, err := s.Struct.Ptr(0)
	return Pin_List{List: p.List()}, err
}

func (s PinEntry) HasPins() bool {
	p, err := s.Struct.Ptr(0)
	return p.IsValid() || err != nil
}

func (s PinEntry) SetPins(v Pin_List) error {
	return s.Struct.SetPtr(0, v.List.ToPtr())
}

// NewPins sets the pins field to a newly
// allocated Pin_List, preferring placement in s's segment.
func (s PinEntry) NewPins(n int32) (Pin_List, error) {
	l, err := NewPin_List(s.Struct.Segment(), n)
	if err != nil {
		return Pin_List{}, err
	}
	err = s.Struct.SetPtr(0, l.List.ToPtr())
	return l, err
}

// PinEntry_List is a list of PinEntry.
type PinEntry_List struct{ capnp.List }

// NewPinEntry creates a new list of PinEntry.
func NewPinEntry_List(s *capnp.Segment, sz int32) (PinEntry_List, error) {
	l, err := capnp.NewCompositeList(s, capnp.ObjectSize{DataSize: 0, PointerCount: 1}, sz)
	return PinEntry_List{l}, err
}

func (s PinEntry_List) At(i int) PinEntry { return PinEntry{s.List.Struct(i)} }

func (s PinEntry_List) Set(i int, v PinEntry) error { return s.List.SetStruct(i, v.Struct) }

func (s PinEntry_List) String() string {
	str, _ := text.MarshalList(0xdb74f7cf7bc815c6, s.List)
	return str
}

// PinEntry_Promise is a wrapper for a PinEntry promised by a client call.
type PinEntry_Promise struct{ *capnp.Pipeline }

func (p PinEntry_Promise) Struct() (PinEntry, error) {
	s, err := p.Pipeline.Struct()
	return PinEntry{s}, err
}

const schema_ba762188b0a6e4cf = "x\xda\\\xd01H\xebP\x14\x06\xe0\xff\xbfI_[" +
	"x\xef\xb55*\x14\x94F\xa8CE\xad\xd5\xa5\xb8\xd8" +
	"\x0e.N\xb9:+\x844\x95\x80\xdc\x84\xe4\xa2\x14g" +
	"A\\\xa5\xe0\xa4\x9b\xb3\xb3\xe0\xa8\xe8\xd4\xcd\xc5\xc5\xc1" +
	"\xc9\xc1\xd51\x92\xa5\x05\xb7\xc3\xcf\xcf\xf9\x0e\xa7|\xdd" +
	"\x11\xad\\(\x009\x97\xfb\x93\x0e\xbf\xf4\xec\xfb\xde\xfe" +
	"\x15d\x95\"\x1d}\xdc\xde\x9d/\x1c\xdf\xc3\xcc\x03\xd6" +
	"<?\xad\x06\xb3i\x91'`\xfa4\xf3|:\xfa\xd6" +
	"o\xa8T9\xa9\xe6\xb2\xc6\xc6\x19\xa7h\x0d\x99\xb7\x86" +
	"\xacY/\xdcB;\xf5\\\xddO\x9a\x9e+\"\x155" +
	"\xa3@)?^\xf5\xdcHE\xa5M'P\x0e)\x0b" +
	"\x86\x09\x98\x04*\x8du@\xd6\x0d\xca5\xc1\x0a;\xd3" +
	"\xcc\xc2\x95\x1d@.\x1b\x94m\xc1Z\xa0\xc2\x9e\xcf\"" +
	"\x04\x8b`\x1a$N\xb6\xb0\x07\x80\x84 \xc1\xb1g\xfc" +
	"\xf62n[\xe9\x98\x83\x0c5)\xd2\x83\xcb\x1b\xf9\xf0" +
	"z\xf1\x08i\x0av\xeb\xe4_\xa0\xc5]\xa6];\x09" +
	"\xd4\xe1\x91o\xda\xbe\xd2\xf1\xc0\xee\x87\xb1\xed\xda\x9e\x1f" +
	"k7P\xb6\x17*\xed+m\xab\xb0G\x1f\x90\xe6\xf8" +
	"\xfe\x7fK\x80,\x18\x94u\xc1R\x14\xa8\x84\xffA\xc7" +
	" \xcb\x93\x0f\x83Y\xf8\x13\x00\x00\xff\xff\x09Mdx"

func init() {
	schemas.Register(schema_ba762188b0a6e4cf,
		0x985d53e01674ee95,
		0xdb74f7cf7bc815c6)
}
