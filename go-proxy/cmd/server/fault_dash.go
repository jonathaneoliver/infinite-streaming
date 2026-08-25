package main

import (
	"encoding/xml"
	"fmt"
	"strings"
)

// DASH MPD parsing for the structure-driven rendition model (#926). go-live
// serves SegmentList MPDs whose SegmentURL/Initialization paths are the SAME
// CMAF segment dirs HLS uses (/720p/…, /audio/…), so once the .mpd is parsed the
// existing dir-keyed _rendition_map (and the classifier that reads it) handles
// DASH segments unchanged. Only the map-population source differs: the MPD
// instead of HLS media playlists.

type mpdManifest struct {
	XMLName xml.Name    `xml:"MPD"`
	Periods []mpdPeriod `xml:"Period"`
}

type mpdPeriod struct {
	AdaptationSets []mpdAdaptationSet `xml:"AdaptationSet"`
}

type mpdAdaptationSet struct {
	ContentType     string              `xml:"contentType,attr"`
	MimeType        string              `xml:"mimeType,attr"`
	Representations []mpdRepresentation `xml:"Representation"`
}

type mpdRepresentation struct {
	ID          string          `xml:"id,attr"`
	Bandwidth   int             `xml:"bandwidth,attr"`
	Width       int             `xml:"width,attr"`
	Height      int             `xml:"height,attr"`
	Codecs      string          `xml:"codecs,attr"`
	MimeType    string          `xml:"mimeType,attr"`
	SegmentList *mpdSegmentList `xml:"SegmentList"`
}

type mpdSegmentList struct {
	Initialization mpdInit         `xml:"Initialization"`
	SegmentURLs    []mpdSegmentURL `xml:"SegmentURL"`
}

type mpdInit struct {
	SourceURL string `xml:"sourceURL,attr"`
}

type mpdSegmentURL struct {
	Media string `xml:"media,attr"`
}

// parseDASHManifest parses an MPD into the video variant ladder (for
// manifest_variants + the dashboard) and the full segmentDir → rendition map.
// Video vs audio comes from the AdaptationSet's contentType / the
// Representation's mimeType — manifest structure, not URL spelling. Returns
// nil, nil on a body that isn't a parseable MPD.
func parseDASHManifest(body []byte) (videoVariants []PlaylistInfo, renditions map[string]renditionInfo) {
	var mpd mpdManifest
	if err := xml.Unmarshal(body, &mpd); err != nil || len(mpd.Periods) == 0 {
		return nil, nil
	}
	renditions = map[string]renditionInfo{}
	// A looping live MPD accumulates one <Period> per loop over the live window,
	// each carrying the SAME Representation ladder. The variant ladder is the
	// SET of distinct renditions, not one entry per Period — so dedup by segment
	// dir (the CMAF rendition key) and keep the first occurrence of each rung.
	seenVideo := map[string]bool{}
	for _, period := range mpd.Periods {
		for _, as := range period.AdaptationSets {
			for _, rep := range as.Representations {
				dir := dashRepDir(rep)
				if dir == "" {
					continue
				}
				if dashRepIsAudio(as, rep) {
					renditions[dir] = renditionInfo{Kind: "audio"}
					continue
				}
				if seenVideo[dir] {
					continue
				}
				seenVideo[dir] = true
				res := ""
				if rep.Width > 0 && rep.Height > 0 {
					res = fmt.Sprintf("%dx%d", rep.Width, rep.Height)
				}
				videoVariants = append(videoVariants, PlaylistInfo{URL: dir, Bandwidth: rep.Bandwidth, Resolution: res})
			}
		}
	}
	// Rung = bandwidth-ascending position across the collected video variants.
	for idx, v := range videoVariants {
		renditions[v.URL] = renditionInfo{
			Kind: "video", Resolution: v.Resolution, BandwidthBps: v.Bandwidth, Rung: rungIndexOf(videoVariants, idx),
		}
	}
	if len(renditions) == 0 {
		return nil, nil
	}
	return videoVariants, renditions
}

// dashRepIsAudio reports whether a Representation is audio, by the
// AdaptationSet contentType or the Representation/AdaptationSet mimeType.
func dashRepIsAudio(as mpdAdaptationSet, rep mpdRepresentation) bool {
	if strings.HasPrefix(strings.ToLower(as.ContentType), "audio") {
		return true
	}
	mime := rep.MimeType
	if mime == "" {
		mime = as.MimeType
	}
	return strings.HasPrefix(strings.ToLower(mime), "audio")
}

// dashRepDir returns the rendition directory a Representation's segments live
// under, from its SegmentList Initialization / SegmentURL paths.
func dashRepDir(rep mpdRepresentation) string {
	if rep.SegmentList == nil {
		return ""
	}
	if rep.SegmentList.Initialization.SourceURL != "" {
		return renditionDirOf(rep.SegmentList.Initialization.SourceURL)
	}
	for _, su := range rep.SegmentList.SegmentURLs {
		if su.Media != "" {
			return renditionDirOf(su.Media)
		}
	}
	return ""
}

// mergeRenditionMap records every dir→rendition entry on the session.
func mergeRenditionMap(session SessionData, renditions map[string]renditionInfo) {
	for dir, info := range renditions {
		setRenditionDir(session, dir, info)
	}
}
