package generator

import (
	"github.com/bluenviron/gohlslib/pkg/playlist"
	"github.com/boss/go-live/pkg/parser"
)

type ContentInfo struct {
	SegmentDuration  float64
	PartialDuration  float64
	FragmentCount    int
	ContentType      string // e.g. "6x1s_llhls", "1x2s_abr"
}

func DetectContent(pl *playlist.Media, byteranges map[string][]parser.ByteRange) ContentInfo {
	if len(pl.Segments) == 0 || pl.Segments[0] == nil {
		return ContentInfo{}
	}
	seg := pl.Segments[0]
	segmentDuration := seg.Duration.Seconds()
	fragmentCount := len(byteranges[seg.URI])
	partialDuration := 0.0
	if fragmentCount > 0 {
		partialDuration = segmentDuration / float64(fragmentCount)
	}
	ctype := ""
	if segmentDuration == 6.0 && fragmentCount == 6 {
		ctype = "6x1s_llhls"
	} else if segmentDuration == 2.0 && fragmentCount == 1 {
		ctype = "1x2s_abr"
	} else {
		ctype = "custom"
	}
	return ContentInfo{
		SegmentDuration: segmentDuration,
		PartialDuration: partialDuration,
		FragmentCount: fragmentCount,
		ContentType: ctype,
	}
}
