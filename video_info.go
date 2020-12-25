package ytdl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/antchfx/jsonquery"
	"github.com/rs/zerolog/log"
)

const youtubeBaseURL = "https://www.youtube.com/watch"
const youtubeEmbeddedBaseURL = "https://www.youtube.com/embed/"
const youtubeVideoEURL = "https://youtube.googleapis.com/v/"
const youtubeVideoInfoURL = "https://www.youtube.com/get_video_info"
const youtubeDateFormat = "2006-01-02"

// VideoInfo contains the info a youtube video
type VideoInfo struct {
	ID              string     // The video ID
	Title           string     // The video title
	Description     string     // The video description
	DatePublished   time.Time  // The date the video was published
	Formats         FormatList // Formats the video is available in
	DASHManifestURL string     // URI of the DASH manifest file
	HLSManifestURL  string     // URI of the HLS manifest file
	Keywords        []string   // List of keywords associated with the video
	Uploader        string     // Author of the video
	Song            string
	Artist          string
	Album           string
	Writers         string
	Duration        time.Duration // Duration of the video
	htmlPlayerFile  string
}

// GetVideoInfo fetches info from a url string, url object, or a url string
func GetVideoInfo(cx context.Context, value interface{}) (*VideoInfo, error) {
	return DefaultClient.GetVideoInfo(cx, value)
}

// GetVideoInfo fetches info from a url string, url object, or a url string
func (c *Client) GetVideoInfo(cx context.Context, value interface{}) (*VideoInfo, error) {
	switch t := value.(type) {
	case *url.URL:
		videoID := extractVideoID(t)
		if len(videoID) == 0 {
			return nil, fmt.Errorf("invalid youtube URL, no video id")
		}
		return c.GetVideoInfoFromID(cx, videoID)
	case string:
		if strings.HasPrefix(t, "https://") {
			uri, err := url.ParseRequestURI(t)
			if err != nil {
				return nil, err
			}
			return c.GetVideoInfo(cx, uri)
		}
		return c.GetVideoInfoFromID(cx, t)
	default:
		return nil, fmt.Errorf("Identifier type must be a string, *url.URL, or []byte")
	}
}

// GetVideoInfoFromShortURL fetches video info from a short youtube url
func extractVideoID(u *url.URL) string {
	switch u.Host {
	case "www.youtube.com", "youtube.com", "m.youtube.com":
		if u.Path == "/watch" {
			return u.Query().Get("v")
		}
		if strings.HasPrefix(u.Path, "/embed/") {
			return u.Path[7:]
		}
	case "youtu.be":
		if len(u.Path) > 1 {
			return u.Path[1:]
		}
	}
	return ""
}

// GetVideoInfoFromID fetches video info from a youtube video id
func (c *Client) GetVideoInfoFromID(cx context.Context, id string) (*VideoInfo, error) {
	body, err := c.httpGetAndCheckResponseReadBody(cx, youtubeBaseURL+"?v="+id+"&gl=US&hl=en&has_verified=1&bpctr=9999999999")

	if err != nil {
		return nil, err
	}
	return c.getVideoInfoFromHTML(cx, id, body)
}

// GetDownloadURL gets the download url for a format
func (c *Client) GetDownloadURL(cx context.Context, info *VideoInfo, format *Format) (*url.URL, error) {
	return c.getDownloadURL(cx, format, info.htmlPlayerFile)
}

// GetThumbnailURL returns a url for the thumbnail image
// with the given quality
func (info *VideoInfo) GetThumbnailURL(quality ThumbnailQuality) *url.URL {
	u, _ := url.Parse(fmt.Sprintf("http://img.youtube.com/vi/%s/%s.jpg",
		info.ID, quality))
	return u
}

// Download is a convenience method to download a format to an io.Writer
func (c *Client) Download(cx context.Context, info *VideoInfo, format *Format, dest io.Writer) error {
	u, err := c.GetDownloadURL(cx, info, format)
	if err != nil {
		return err
	}

	resp, err := c.httpGetAndCheckResponse(cx, u.String())
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	_, err = io.Copy(dest, resp.Body)
	return err
}

var (
	regexpPlayerConfig          = regexp.MustCompile("ytplayer\\.config = (.*?);ytplayer\\.")
	regexpPlayerConfigEmbedded  = regexp.MustCompile("yt.setConfig\\({'PLAYER_CONFIG': (.*?)}\\);")
	regexpInitialData           = regexp.MustCompile(`\["ytInitialData"\] = (.+);`)
	regexpInitialPlayerResponse = regexp.MustCompile(`\["ytInitialPlayerResponse"\] = (.+);`)
)

func (c *Client) getVideoInfoFromHTML(cx context.Context, id string, html []byte) (*VideoInfo, error) {
	info := &VideoInfo{}

	if matches := regexpInitialData.FindSubmatch(html); len(matches) > 0 {
		if err := info.addMetadata(matches[1]); err != nil {
			log.Debug().Msgf("Unable to parse metadata rows: %v", err)
		}
	}

	info.ID = id

	var jsonConfig playerConfig

	// match json in javascript
	if matches := regexpPlayerConfig.FindSubmatch(html); len(matches) > 1 {
		err := json.Unmarshal(matches[1], &jsonConfig)
		if err != nil {
			return nil, err
		}
	} else {
		log.Debug().Msg("Unable to extract json from default url, trying embedded url")

// 		embeddedInfo, err := c.getPlayerConfigFromEmbedded(cx, id)
// 		if err != nil {
// 			return nil, err
// 		}

// 		info.htmlPlayerFile = embeddedInfo.Assets.JS

		query := url.Values{
			"video_id": []string{id},
			"eurl":     []string{youtubeVideoEURL + id},
		}

		body, err := c.httpGetAndCheckResponseReadBody(cx, youtubeVideoInfoURL+"?"+query.Encode())
		if err != nil {
			return nil, fmt.Errorf("Unable to read video info: %w", err)
		}

		query, err = url.ParseQuery(string(body))
		if err != nil {
			return nil, fmt.Errorf("Unable to parse video info data: %w", err)
		}

		for k, v := range query {
			switch k {
			case "errorcode":
				jsonConfig.Args.Errorcode = v[0]
			case "reason":
				jsonConfig.Args.Reason = v[0]
			case "status":
				jsonConfig.Args.Status = v[0]
			case "player_response":
				jsonConfig.Args.PlayerResponse = v[0]
			case "url_encoded_fmt_stream_map":
				jsonConfig.Args.URLEncodedFmtStreamMap = v[0]
			case "adaptive_fmts":
				jsonConfig.Args.AdaptiveFmts = v[0]
			case "dashmpd":
				jsonConfig.Args.Dashmpd = v[0]
			default:
				// log.Debug().Msgf("unknown query param: %v", k)
			}
		}
	}

	inf := jsonConfig.Args
	if inf.Status == "fail" {
		return nil, fmt.Errorf("Error %s:%s", inf.Errorcode, inf.Reason)
	}

	var formats FormatList
	c.addFormatsByQueryStrings(&formats, strings.NewReader(inf.URLEncodedFmtStreamMap), false)
	if inf.AdaptiveFmts != "" {
		c.addFormatsByQueryStrings(&formats, strings.NewReader(inf.AdaptiveFmts), true)
	}
	if inf.PlayerResponse != "" {
		response := &playerResponse{}

		if err := json.Unmarshal([]byte(inf.PlayerResponse), &response); err != nil {
			return nil, fmt.Errorf("Couldn't parse player response: %w", err)
		}
		info.DASHManifestURL = response.StreamingData.DashManifestUrl
		info.HLSManifestURL = response.StreamingData.HlsManifestUrl
		if response.PlayabilityStatus.Status != "OK" {
			return nil, fmt.Errorf("Unavailable because: %s", response.PlayabilityStatus.Reason)
		}

		c.addFormatsByInfos(&formats, response.StreamingData.Formats, false)
		c.addFormatsByInfos(&formats, response.StreamingData.AdaptiveFormats, true)
		if seconds := response.VideoDetails.LengthSeconds; seconds != "" {
			val, err := strconv.Atoi(seconds)
			if err == nil {
				info.Duration = time.Duration(val) * time.Second
			}
		}

		if date, err := time.Parse(youtubeDateFormat, response.Microformat.Renderer.PublishDate); err == nil {
			info.DatePublished = date
		} else {
			log.Debug().Msgf("Unable to parse date published %v", err)
		}

		info.Title = response.VideoDetails.Title
		info.Uploader = response.VideoDetails.Author
	} else {
		log.Debug().Msg("Unable to extract player response JSON")
	}

	if info.htmlPlayerFile == "" {
		info.htmlPlayerFile = jsonConfig.Assets.JS
	}

	if len(formats) == 0 {
		log.Debug().Msgf("No formats found")
	}

	info.Formats = formats
	return info, nil
}

func (info *VideoInfo) addMetadata(jsondata []byte) error {
	doc, err := jsonquery.Parse(bytes.NewReader(jsondata))
	if err != nil {
		return err
	}

	// Extract description
	descr, err := jsonquery.Query(doc, "//videoSecondaryInfoRenderer/description")
	if err != nil {
		return err
	}
	if descr != nil {
		var sb strings.Builder
		for _, child := range jsonquery.Find(descr, "//text") {
			sb.WriteString(child.InnerText())
		}
		info.Description = sb.String()
	}

	// Extract metadata rows
	rows := jsonquery.Find(doc, "//metadataRowContainer/*/rows/*/metadataRowRenderer")
	for _, row := range rows {
		key, value := getMetaDataRow(row)

		switch key {
		case "Artist":
			info.Artist = value
		case "Song":
			info.Song = value
		case "Album":
			info.Album = value
		case "Writers":
			info.Writers = value
		}
	}
	return nil
}

func (c *Client) addFormatsByInfos(formats *FormatList, infos []formatInfo, adaptive bool) {
	for _, info := range infos {
		if err := formats.addByInfo(info, adaptive); err != nil {
			log.Debug().Err(err)
		}
	}
}

func (c *Client) addFormatsByQueryStrings(formats *FormatList, rd io.Reader, adaptive bool) {
	r := bufio.NewReader(rd)
	for {
		line, err := r.ReadString(',')
		if err == io.EOF {
			break
		}
		if err := formats.addByQueryString(line[:len(line)-1], adaptive); err != nil {
			log.Debug().Err(err)
		}
	}
}

func (c *Client) getPlayerConfigFromEmbedded(cx context.Context, id string) (*playerConfig, error) {

	html, err := c.httpGetAndCheckResponseReadBody(cx, youtubeEmbeddedBaseURL+id)

	if err != nil {
		return nil, fmt.Errorf("Embedded url request returned %w", err)
	}

	matches := regexpPlayerConfigEmbedded.FindSubmatch(html)
	if len(matches) < 2 {
		return nil, fmt.Errorf("Error extracting sts from embedded url response")
	}

	config := &playerConfig{}
	dec := json.NewDecoder(bytes.NewBuffer(matches[1]))
	err = dec.Decode(config)
	if err != nil {
		return nil, fmt.Errorf("Unable to extract json from embedded url: %w", err)
	}

	return config, nil
}
