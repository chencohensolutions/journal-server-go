package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
)

// tiffExif reads the EXIF dates and GPS position of a TIFF-based file (DNG), which vipsheader doesn't show
// for them, as the fields vipsheader names for JPEG and HEIC ("exif-ifd2-DateTimeOriginal": "2025:12:04
// 14:28:36", "exif-ifd3-GPSLatitude": "32/1 50/1 49947000/1000000").
func tiffExif(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	header := make([]byte, 8)
	if _, err := f.ReadAt(header, 0); err != nil {
		return nil, err
	}
	var order binary.ByteOrder
	switch string(header[:4]) {
	case "II*\x00":
		order = binary.LittleEndian
	case "MM\x00*":
		order = binary.BigEndian
	default:
		return nil, errors.New("not a TIFF file")
	}

	type entry struct {
		typ   uint16
		count uint32
		data  []byte
	}
	readIFD := func(offset int64) (map[uint16]entry, error) {
		count := make([]byte, 2)
		if _, err := f.ReadAt(count, offset); err != nil {
			return nil, err
		}
		n := int(order.Uint16(count))
		raw := make([]byte, n*12)
		if _, err := f.ReadAt(raw, offset+2); err != nil {
			return nil, err
		}
		entries := map[uint16]entry{}
		for i := 0; i < n; i++ {
			e := raw[i*12:]
			tag, typ, num := order.Uint16(e), order.Uint16(e[2:]), order.Uint32(e[4:])
			size := map[uint16]uint32{1: 1, 2: 1, 3: 2, 4: 4, 5: 8, 7: 1}[typ]
			if size == 0 || num == 0 || num > 1<<16 {
				continue
			}
			data := e[8:12]
			if size*num > 4 {
				data = make([]byte, size*num)
				if _, err := f.ReadAt(data, int64(order.Uint32(e[8:]))); err != nil {
					continue
				}
			} else {
				data = data[:size*num]
			}
			entries[tag] = entry{typ, num, data}
		}
		return entries, nil
	}
	pointer := func(e entry) (int64, bool) {
		switch {
		case e.typ == 4 && len(e.data) >= 4:
			return int64(order.Uint32(e.data)), true
		case e.typ == 3 && len(e.data) >= 2:
			return int64(order.Uint16(e.data)), true
		}
		return 0, false
	}

	fields := map[string]string{}
	set := func(name string, e entry, ok bool) {
		if !ok {
			return
		}
		switch e.typ {
		case 2:
			if s := strings.TrimSpace(strings.TrimRight(string(e.data), "\x00")); s != "" {
				fields[name] = s
			}
		case 5:
			parts := make([]string, 0, e.count)
			for i := 0; i+8 <= len(e.data); i += 8 {
				parts = append(parts, fmt.Sprintf("%d/%d", order.Uint32(e.data[i:]), order.Uint32(e.data[i+4:])))
			}
			fields[name] = strings.Join(parts, " ")
		}
	}

	ifd0, err := readIFD(int64(order.Uint32(header[4:])))
	if err != nil {
		return nil, err
	}
	for tag, name := range map[uint16]string{0x010F: "Make", 0x0110: "Model", 0x0131: "Software", 0x0132: "DateTime"} {
		e, ok := ifd0[tag]
		set("exif-ifd0-"+name, e, ok)
	}
	if p, ok := pointer(ifd0[0x8769]); ok {
		if exif, err := readIFD(p); err == nil {
			for tag, name := range map[uint16]string{
				0x9003: "DateTimeOriginal", 0x9004: "DateTimeDigitized",
				0x9010: "OffsetTime", 0x9011: "OffsetTimeOriginal", 0x9012: "OffsetTimeDigitized",
			} {
				e, ok := exif[tag]
				set("exif-ifd2-"+name, e, ok)
			}
		}
	}
	if p, ok := pointer(ifd0[0x8825]); ok {
		if gps, err := readIFD(p); err == nil {
			for tag, name := range map[uint16]string{1: "GPSLatitudeRef", 2: "GPSLatitude", 3: "GPSLongitudeRef", 4: "GPSLongitude"} {
				e, ok := gps[tag]
				set("exif-ifd3-"+name, e, ok)
			}
		}
	}
	return fields, nil
}
