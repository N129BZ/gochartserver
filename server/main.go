package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"database/sql"

	_ "github.com/mattn/go-sqlite3"
)

type SettingMessage struct {
	Setting string `json:"setting"`
	Value   bool   `json:"state"`
}

type XmlWrapper struct {
	RawXML string `json:"raw_xml"`
}

type MbTileConnectionCacheEntry struct {
	Path     string
	Conn     *sql.DB
	Metadata map[string]string
	fileTime time.Time
}

// Begin type definitions

// Frequency represents a frequency list for an Airport
type Frequency struct {
	Frequency   float64 `json:"frequency"`
	Description string  `json:"description"`
}

// Runway data...
type Runway struct {
	Length  int    `json:"length"`
	Width   int    `json:"width"`
	Surface string `json:"surface"`
	LeIdent string `json:"le_ident"`
	HeIdent string `json:"he_ident"`
}

// Airport data...
type Airport struct {
	Ident       string      `json:"ident"`
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Lon         float64     `json:"lon"`
	Lat         float64     `json:"lat"`
	Elevation   int         `json:"elevation"`
	Frequencies []Frequency `json:"frequencies"`
	Runways     []Runway    `json:"runways"`
}

type Navaid struct {
	Ident              string  `json:"ident"`
	Name               string  `json:"name"`
	Type               string  `json:"type"`
	FrequencyKhz       int     `json:"frequency_khz"`
	LatitudeDeg        float64 `json:"latitude_deg"`
	LongitudeDeg       float64 `json:"longitude_deg"`
	ElevationFt        string  `json:"elevation_ft"`
	Dme_frequency_khz  string  `json:"dme_frequency_khz"`
	Dme_channel        string  `json:"dme_channel"`
	Dme_latitude_deg   string  `json:"dme_latitude_deg"`
	Dme_longitude_deg  string  `json:"dme_longitude_deg"`
	Dme_elevation_ft   string  `json:"dme_elevation_ft"`
	Slaved_var_deg     string  `json:"slaved_variation_deg"`
	Mag_var_deg        float64 `json:"magnetic_variation_deg"`
	UsageType          string  `json:"usageType"`
	Power              string  `json:"power"`
	Associated_airport string  `json:"associated_airport"`
}

// Airport cache entry
// Generic map database connection cache entry
type MbMapConnectionCacheEntry struct {
	Path     string
	Conn     *sql.DB
	fileTime time.Time
}

var MAIN_HOME = "/home/bro/sources/gochartserver"
var MAIN_WWW_DIR = MAIN_HOME + "/dist/"

// create a mutex for the map DB cache
var mbMapCacheLock = sync.Mutex{}

// map DB connection cache
var mbMapCache = make(map[string]MbMapConnectionCacheEntry)

var ManagementAddr = 8500

// Source represents the type of file to download
type Source struct {
	Type  string
	Token string
}

func NewMapConnectionCacheEntry(path string, conn *sql.DB) *MbMapConnectionCacheEntry {
	file, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &MbMapConnectionCacheEntry{path, conn, file.ModTime()}
}
func (this *MbMapConnectionCacheEntry) IsOutdated() bool {
	file, err := os.Stat(this.Path)
	if err != nil {
		return true
	}
	modTime := file.ModTime()
	return modTime != this.fileTime
}
//  End of Airport types

func (this *MbTileConnectionCacheEntry) IsOutdated() bool {
	file, err := os.Stat(this.Path)
	if err != nil {
		return true
	}
	modTime := file.ModTime()
	return modTime != this.fileTime
}

func NewMbTileConnectionCacheEntry(path string, conn *sql.DB) *MbTileConnectionCacheEntry {
	file, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return &MbTileConnectionCacheEntry{path, conn, nil, file.ModTime()}
}

var mbtileCacheLock = sync.Mutex{}
var mbtileConnectionCache = make(map[string]MbTileConnectionCacheEntry)

/* func handleJsonIo(conn *websocket.Conn) {
	// Connection closes when function returns. Since uibroadcast is writing and we don't need to read anything (for now), just keep it busy.
	for {
		buf := make([]byte, 1024)
		_, err := conn.Read(buf)
		if err != nil {
			break
		}
		if buf[0] != 0 { // Dummy.
			continue
		}
		time.Sleep(1 * time.Second)
	}
} */

func setNoCache(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func setJSONHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
}

func defaultServer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "max-age=360") // 5 min, so that if user installs update, he will revalidate soon enough
	//	setNoCache(w)
	if r.URL.Path == "/" || r.URL.Path == "" {
		http.ServeFile(w, r, filepath.Join(MAIN_WWW_DIR, "map.html"))
		return
	}
	http.FileServer(http.Dir(MAIN_WWW_DIR)).ServeHTTP(w, r)
}

func tileToDegree(z, x, y int) (lon, lat float64) {
	// osm-like schema:
	y = (1 << z) - y - 1
	n := math.Pi - 2.0*math.Pi*float64(y)/math.Exp2(float64(z))
	lat = 180.0 / math.Pi * math.Atan(0.5*(math.Exp(n)-math.Exp(-n)))
	lon = float64(x)/math.Exp2(float64(z))*360.0 - 180.0
	return lon, lat
}

func readMbTilesMetadata(fname string, db *sql.DB) map[string]string {
	rows, err := db.Query(`SELECT name, value FROM metadata 
		UNION SELECT 'minzoom', min(zoom_level) FROM tiles WHERE NOT EXISTS (SELECT * FROM metadata WHERE name='minzoom' and value is not null and value != '')
		UNION SELECT 'maxzoom', max(zoom_level) FROM tiles WHERE NOT EXISTS (SELECT * FROM metadata WHERE name='maxzoom' and value is not null and value != '')`)
	if err != nil {
		log.Printf("SQLite read error %s: %s", fname, err.Error())
		return nil
	}
	defer rows.Close()
	meta := make(map[string]string)
	for rows.Next() {
		var name, val string
		rows.Scan(&name, &val)
		if len(val) > 0 {
			meta[name] = val
		}
	}
	// determine extent of layer if not given.. Openlayers kinda needs this, or it can happen that it tries to do
	// a billion request do down-scale high-res pngs that aren't even there (i.e. all 404s)
	if _, ok := meta["bounds"]; !ok {
		maxZoomInt, _ := strconv.ParseInt(meta["maxzoom"], 10, 32)
		rows, err = db.Query("SELECT min(tile_column), min(tile_row), max(tile_column), max(tile_row) FROM tiles WHERE zoom_level=?", maxZoomInt)
		if err != nil {
			log.Printf("SQLite read error %s: %s", fname, err.Error())
			return nil
		}
		rows.Next()
		var xmin, ymin, xmax, ymax int
		rows.Scan(&xmin, &ymin, &xmax, &ymax)
		lonmin, latmin := tileToDegree(int(maxZoomInt), xmin, ymin)
		lonmax, latmax := tileToDegree(int(maxZoomInt), xmax+1, ymax+1)
		meta["bounds"] = fmt.Sprintf("%f,%f,%f,%f", lonmin, latmin, lonmax, latmax)
	}

	// check if it is vectortiles and we have a style, then add the URL to metadata...
	if format, ok := meta["format"]; ok && format == "pbf" {
		_, file := filepath.Split(fname)
		if _, err := os.Stat(MAIN_HOME + "/mapdata/styles/" + file + "/style.json"); err == nil {
			// We found a style!
			meta["stratux_style_url"] = "/mapdata/styles/" + file + "/style.json"
		}

	}
	return meta
}

// Scans mapdata dir for all .db and .mbtiles files and returns json representation of all metadata values
func handleTilesets(w http.ResponseWriter, r *http.Request) {
	files, err := os.ReadDir(MAIN_HOME + "/tilesets/")
	if err != nil {
		log.Printf("handleTilesets() error: %s\n", err.Error())
		http.Error(w, err.Error(), 500)
	}
	result := make(map[string]map[string]string, 0)
	for _, f := range files {
		if f.IsDir() {
			continue
		}
		if strings.HasSuffix(f.Name(), ".mbtiles") || strings.HasSuffix(f.Name(), ".db") {
			_, meta, err := connectMbTilesArchive(MAIN_HOME + "/tilesets/" + f.Name())
			if err != nil {
				log.Printf("SQLite open "+f.Name()+" failed: %s", err.Error())
				continue
			}
			result[f.Name()] = meta
		}
	}
	resJson, _ := json.Marshal(result)
	w.Write(resJson)
}

func loadTile(fname string, z, x, y int) ([]byte, error) {
	db, meta, err := connectMbTilesArchive(MAIN_HOME + "/tilesets/" + fname)
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT tile_data FROM tiles WHERE zoom_level=? AND tile_column=? AND tile_row=?", z, x, y)
	if err != nil {
		log.Printf("Failed to query mbtiles: %s", err.Error())
		return nil, nil
	}

	defer rows.Close()
	for rows.Next() {
		var res []byte
		rows.Scan(&res)
		// sometimes pbfs are gzipped...
		if format, ok := meta["format"]; ok && format == "pbf" && len(res) >= 2 && res[0] == 0x1f && res[1] == 0x8b {
			reader := bytes.NewReader(res)
			gzreader, _ := gzip.NewReader(reader)
			unzipped, err := io.ReadAll(gzreader)
			if err != nil {
				log.Printf("Failed to unzip gzipped PBF data")
				return nil, nil
			}
			res = unzipped
		}
		return res, nil
	}
	return nil, nil
}

func handleTile(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(r.RequestURI, "/")
	if len(parts) < 4 {
		return
	}
	idx := len(parts) - 1
	y, err := strconv.Atoi(strings.Split(parts[idx], ".")[0])
	if err != nil {
		http.Error(w, "Failed to parse y", 500)
		return
	}
	idx--
	x, _ := strconv.Atoi(parts[idx])
	idx--
	z, _ := strconv.Atoi(parts[idx])
	idx--
	file, _ := url.QueryUnescape(parts[idx])
	tileData, err := loadTile(file, z, x, y)
	if err != nil {
		http.Error(w, err.Error(), 500)
	} else if tileData == nil {
		http.Error(w, "Tile not found", 200) // 404 would make the browser retry forever
	} else {
		w.Write(tileData)
	}
}

func handleAirportListRequest(w http.ResponseWriter, r *http.Request) {
	// Get bounding box parameters
	getclosed := r.URL.Query().Get("getclosed")
	getheliports := r.URL.Query().Get("getheliports")

	includeClosedAirports := false
	if getclosed != "" {
		includeClosedAirports, _ = strconv.ParseBool(getclosed)
	}
	includeHeliports := false
	if getheliports != "" {
		includeHeliports, _ = strconv.ParseBool(getheliports)
	}

	minLatStr := r.URL.Query().Get("minLat")
	maxLatStr := r.URL.Query().Get("maxLat")
	minLonStr := r.URL.Query().Get("minLon")
	maxLonStr := r.URL.Query().Get("maxLon")

	// Check if we have all required geographic bounds
	if minLatStr == "" || maxLatStr == "" || minLonStr == "" || maxLonStr == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]"))
		return
	}

	// Parse bounding box coordinates
	minLat, err1 := strconv.ParseFloat(minLatStr, 64)
	maxLat, err2 := strconv.ParseFloat(maxLatStr, 64)
	minLon, err3 := strconv.ParseFloat(minLonStr, 64)
	maxLon, err4 := strconv.ParseFloat(maxLonStr, 64)

	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("[]"))
		return
	}

	db, err := connectMapArchive(MAIN_WWW_DIR+"data/airports.db", true)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err != nil {
		w.Write([]byte("[]"))
		return
	}

	// Query airports within geographic bounds with full details
	query := `SELECT ident, name, type, longitude_deg, latitude_deg, elevation_ft FROM airports 
		WHERE latitude_deg BETWEEN ? AND ? AND longitude_deg BETWEEN ? AND ?`

	// Add heliport exclusion if includeHeliports is false
	if includeHeliports == false {
		query += ` AND type NOT LIKE '%heliport%' AND UPPER(name) NOT LIKE '%HELIPORT%'`
	}

	if includeClosedAirports == false {
		query += ` AND type NOT LIKE '%closed%' AND UPPER(name) NOT LIKE '%CLOSED%'`
	}

	// Limit results to 200 airports to avoid overwhelming the client
	query += ` ORDER BY name LIMIT 200`

	rows, err := db.Query(query, minLat, maxLat, minLon, maxLon)
	if err != nil {
		w.Write([]byte("[]"))
		return
	}

	var airports []Airport
	for rows.Next() {
		var a Airport
		var elevation int
		if err := rows.Scan(&a.Ident, &a.Name, &a.Type, &a.Lon, &a.Lat, &elevation); err != nil {
			continue
		}
		a.Elevation = elevation

		// Query frequencies for this airport
		freqRows, err := db.Query(`SELECT frequency_mhz, description FROM frequencies WHERE airport_ident = ?;`, a.Ident)
		if err != nil {
			a.Frequencies = []Frequency{}
		} else {
			defer freqRows.Close()
			var frequencies []Frequency
			for freqRows.Next() {
				var f Frequency
				err := freqRows.Scan(&f.Frequency, &f.Description)
				if err != nil {
					continue
				}
				frequencies = append(frequencies, f)
			}
			a.Frequencies = frequencies
		}
		if a.Frequencies == nil {
			a.Frequencies = []Frequency{}
		}

		// Query runways for this airport
		rwRows, err := db.Query(`SELECT length_ft, width_ft, surface, le_ident, he_ident FROM runways WHERE airport_ident = ?;`, a.Ident)
		if err != nil {
			a.Runways = []Runway{}
		} else {
			defer rwRows.Close()
			var runways []Runway
			for rwRows.Next() {
				var r Runway
				err := rwRows.Scan(&r.Length, &r.Width, &r.Surface, &r.LeIdent, &r.HeIdent)
				if err != nil {
					continue
				}
				runways = append(runways, r)
			}
			a.Runways = runways
		}
		if a.Runways == nil {
			a.Runways = []Runway{}
		}

		airports = append(airports, a)
	}
	json.NewEncoder(w).Encode(airports)
}

func handleAirportRequest(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	id = strings.TrimSpace(id)

	var airport Airport
	db, err := connectMapArchive(MAIN_WWW_DIR+"data/airports.db", true)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err != nil {
		json.NewEncoder(w).Encode(Airport{})
		return
	}

	// Query airport main info
	row := db.QueryRow(`SELECT ident, name, type, longitude_deg, latitude_deg, elevation_ft FROM airports WHERE ident = ?;`, id)
	var lon, lat float64
	var elevation int
	err = row.Scan(&airport.Ident, &airport.Name, &airport.Type, &lon, &lat, &elevation)
	if err == sql.ErrNoRows || err != nil {
		json.NewEncoder(w).Encode(Airport{})
		return
	}
	airport.Lon = lon
	airport.Lat = lat
	airport.Elevation = elevation

	// Query frequencies
	freqRows, err := db.Query(`SELECT frequency_mhz, description FROM frequencies WHERE airport_ident = ?;`, id)
	if err != nil {
		airport.Frequencies = []Frequency{}
	} else {
		defer freqRows.Close()
		var frequencies []Frequency
		for freqRows.Next() {
			var f Frequency
			err := freqRows.Scan(&f.Frequency, &f.Description)
			if err != nil {
				continue
			}
			frequencies = append(frequencies, f)
		}
		airport.Frequencies = frequencies
	}
	if airport.Frequencies == nil {
		airport.Frequencies = []Frequency{}
	}

	// Query runways
	rwRows, err := db.Query(`SELECT length_ft, width_ft, surface, le_ident, he_ident FROM runways WHERE airport_ident = ?;`, id)
	if err != nil {
		airport.Runways = []Runway{}
	} else {
		defer rwRows.Close()
		var runways []Runway
		for rwRows.Next() {
			var r Runway
			err := rwRows.Scan(&r.Length, &r.Width, &r.Surface, &r.LeIdent, &r.HeIdent)
			if err != nil {
				continue
			}
			runways = append(runways, r)
		}
		airport.Runways = runways
	}
	if airport.Runways == nil {
		airport.Runways = []Runway{}
	}

	json.NewEncoder(w).Encode(airport)
}

func handleGetMapstateRequest(w http.ResponseWriter, r *http.Request) {
	db, err := connectMapArchive(MAIN_WWW_DIR+"data/mapstate.db", false)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"db open failed"}`))
		return
	}
	var statejson string
	var timestamp int64
	err = db.QueryRow(`SELECT state, timestamp FROM mapstate LIMIT 1;`).Scan(&statejson, &timestamp)
	if err != nil || statejson == "" || statejson == "null" {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	// Compose a JSON object with both state and timestamp
	w.Write([]byte(fmt.Sprintf(`{"state":%s,"timestamp":%d}`, statejson, timestamp)))
}

func handleSaveMapstatePost(w http.ResponseWriter, r *http.Request) {
	var req map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"invalid json"}`))
		return
	}
	// Use incoming timestamp if present, else use current time
	var ts int64
	if tsv, ok := req["timestamp"]; ok {
		switch v := tsv.(type) {
		case float64:
			ts = int64(v)
		case int64:
			ts = v
		default:
			ts = time.Now().UTC().Unix()
		}
	} else {
		ts = time.Now().UTC().Unix()
	}
	state, err := json.Marshal(req)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"json marshal failed"}`))
		return
	}
	db, err := connectMapArchive(MAIN_WWW_DIR+"data/mapstate.db", false)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"db open failed"}`))
		return
	}
	res, err := db.Exec("UPDATE mapstate SET state = ?, timestamp = ?", string(state), ts)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"db update failed"}`))
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// No row to update, insert new
		_, err = db.Exec("INSERT INTO mapstate (state, timestamp) VALUES (?, ?)", string(state), ts)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"db insert failed"}`))
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"status":"ok"}`))
}

func connectMbTilesArchive(path string) (*sql.DB, map[string]string, error) {
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, nil, err
	}

	meta := make(map[string]string)
	rows, err := db.Query("SELECT name, value FROM metadata")
	if err != nil {
		return db, meta, nil // Return db even if metadata fails
	}
	defer rows.Close()

	for rows.Next() {
		var name, value string
		rows.Scan(&name, &value)
		meta[name] = value
	}

	return db, meta, nil
}

func connectMapArchive(path string, create bool) (*sql.DB, error) {
	if create {
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			return nil, err
		}
		_, err = db.Exec(`CREATE TABLE IF NOT EXISTS mapstate (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			state TEXT,
			timestamp INTEGER
		)`)
		if err != nil {
			return nil, err
		}
		return db, nil
	}
	return sql.Open("sqlite3", path)
}

func viewLogs(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
	w.Write([]byte("Logs not implemented"))
}

func handleNavaidsRequest(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
	w.Write([]byte("Navaids not implemented"))
}

func handleSaveHistoryPost(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNotImplemented)
	w.Write([]byte("Save history not implemented"))
}

func DownloadWeatherData() {
	go DownloadXmlFile("metars")
	go DownloadXmlFile("tafs")
	go DownloadXmlFile("aircraftreports")
}

func DownloadXmlFile(source string) error {

	url := strings.Replace("https://aviationweather.gov/data/cache/###.cache.xml.gz", "###", source, -1)
	log.Println(url);
    resp, err := http.Get(url)
    if err != nil {
        return fmt.Errorf("error getting message type %s: %v", source, err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != 200 {
        return fmt.Errorf("HTTP error: %d for URL %s", resp.StatusCode, url)
    }

    body, err := io.ReadAll(resp.Body)
    if err != nil {
        return fmt.Errorf("error reading response body: %v", err)
    }

    xmlData := body
    if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
        gz, err := gzip.NewReader(bytes.NewReader(body))
        if err != nil {
            return fmt.Errorf("error creating gzip reader: %v", err)
        }
        defer gz.Close()

        xmlData, err = io.ReadAll(gz)
        if err != nil {
            return fmt.Errorf("error decompressing gzip response: %v", err)
        }
    }

    weatherDir := filepath.Join(MAIN_HOME, "weather")
    if err := os.MkdirAll(weatherDir, 0755); err != nil {
        return fmt.Errorf("error creating weather directory: %v", err)
    }

    weatherFile := filepath.Join(weatherDir, source+".xml")
    if err := os.WriteFile(weatherFile, xmlData, 0644); err != nil {
        return fmt.Errorf("error writing weather file: %v", err)
    }
    return nil
}

func handleMetarsRequest(w http.ResponseWriter, r *http.Request) {
	weatherFile := filepath.Join(MAIN_HOME, "weather", "metars.xml")
	data, err := os.ReadFile(weatherFile)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to read weather data"}`))
		return
	}
	
	msg:= XmlWrapper{
		RawXML: string(data),
	}
	json.NewEncoder(w).Encode(msg)
}	

func handleTafsRequest(w http.ResponseWriter, r *http.Request) {
	weatherFile := filepath.Join(MAIN_HOME, "weather", "tafs.xml")
	data, err := os.ReadFile(weatherFile)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to read weather data"}`))
		return
	}
	
	msg:= XmlWrapper{
		RawXML: string(data),
	}
	json.NewEncoder(w).Encode(msg)
}	

func handlePirepsRequest(w http.ResponseWriter, r *http.Request) {
	weatherFile := filepath.Join(MAIN_HOME, "weather", "aircraftreports.xml")
	data, err := os.ReadFile(weatherFile)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"failed to read weather data"}`))
		return
	}

	msg := XmlWrapper{
		RawXML: string(data),
	}
	json.NewEncoder(w).Encode(msg)
}	

func main() {
	http.HandleFunc("/", defaultServer)
	http.Handle("/tilesets/styles/", http.StripPrefix("/tilesets/styles/", http.FileServer(http.Dir(MAIN_HOME+"/mapdata/styles"))))
	http.HandleFunc("/logs/", viewLogs)

	/* http.HandleFunc("/jsonio",
		func(w http.ResponseWriter, req *http.Request) {
			s := websocket.Server{
				Handler: websocket.Handler(handleJsonIo)}
			s.ServeHTTP(w, req)
		}) */

	// routes for retrieving tiles, airport data, map state, and position history
	http.HandleFunc("/metars", handleMetarsRequest)
	http.HandleFunc("/tafs", handleTafsRequest)
	http.HandleFunc("/pireps", handlePirepsRequest)
	http.HandleFunc("/airportlist", handleAirportListRequest)
	http.HandleFunc("/navaids", handleNavaidsRequest)
	http.HandleFunc("/airport", handleAirportRequest)
	http.HandleFunc("/savehistory", handleSaveHistoryPost)
	http.HandleFunc("/getmapstate", handleGetMapstateRequest)
	http.HandleFunc("/savemapstate", handleSaveMapstatePost)
	http.HandleFunc("/tiles/tilesets", handleTilesets)
	http.HandleFunc("/tiles/", handleTile)

	addr := fmt.Sprintf(":%d", ManagementAddr)

	log.Printf("web configuration console on port %s", addr)
	
	DownloadWeatherData()
	
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Printf("serverInterface ListenAndServe: %s\n", err.Error())
	}
}
