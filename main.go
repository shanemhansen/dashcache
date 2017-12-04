package main

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/lib/pq"
	"github.com/shanemhansen/dashcache/response"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var backendProm = flag.String("prom-url", "", "url of promethus to query")
var network = flag.String("net", "tcp", "network to listen on")
var addr = flag.String("addr", ":9090", "address to listen on")
var dburl = flag.String("db-url", "", "postgres database to query")
var cache = flag.Bool("cache", true, "whether or not to cache")

type QuerySpec struct {
	Query string
	Start int
	End   int
	Step  time.Duration
}

func (this *QuerySpec) Key() string {
	val := *this
	val.Start = val.Start / 60 * 60
	val.End = val.Start / 60 * 60
	return fmt.Sprintf("%s-%d-%d-%s", val.Query, val.Start, val.End, this.Step)
}

func main() {
	flag.Parse()
	if *dburl == "" {
		log.Fatal("dburl is required")
	}
	dsn, err := pq.ParseURL(*dburl)
	if err != nil {
		log.Fatalf("unable to parse dburl: %s", err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("unable to connect to db: %s", err)
	}
	defer db.Close()
	http.HandleFunc("/", passthrough)
	http.HandleFunc("/api/v1/query_range", func(wtr http.ResponseWriter, req *http.Request) {
		t0 := time.Now()
		defer func() {
			IncrTime(time.Now().Sub(t0), "query_range.duration")
		}()
		q := req.URL.EscapedPath()
		if req.URL.RawQuery != "" {
			q += "?" + req.URL.RawQuery
		}
		prom, err := handleRangeRequest(db, q)
		if err != nil {
			wtr.WriteHeader(http.StatusInternalServerError)
			wtr.Write([]byte(err.Error()))
			return
		}
		var w io.Writer = wtr
		if ae := req.Header.Get("Accept-Encoding"); strings.Contains(ae, "gzip") {
			wtr.Header().Set("Content-Encoding", "gzip")
			gzwtr := gzip.NewWriter(wtr)
			defer gzwtr.Close()
			w = gzwtr
		}
		var body bytes.Buffer
		if err := json.NewEncoder(&body).Encode(prom); err != nil {
			wtr.WriteHeader(http.StatusInternalServerError)
			wtr.Write([]byte(err.Error()))
			return
		}
		w.Write(body.Bytes())
		return
	})
	http.ListenAndServe(*addr, nil)
}
func passthrough(wtr http.ResponseWriter, req *http.Request) {
	// default route just handles basic gets
	u := *backendProm + req.URL.EscapedPath()
	if req.URL.RawQuery != "" {
		u += "?" + req.URL.RawQuery
	}
	outReq, err := http.NewRequest("GET", u, nil)
	if err != nil {
		wtr.WriteHeader(http.StatusInternalServerError)
		wtr.Write([]byte(err.Error()))
		return
	}
	if ae := req.Header.Get("Accept-Encoding"); ae != "" {
		outReq.Header.Set("Accept-Encoding", ae)
	}
	resp, err := http.DefaultClient.Do(outReq)
	if err != nil {
		wtr.WriteHeader(http.StatusInternalServerError)
		wtr.Write([]byte(err.Error()))
		return
	}
	defer resp.Body.Close()
	for key, value := range resp.Header {
		wtr.Header()[key] = value
	}
	io.Copy(wtr, resp.Body)
}

var isNum = regexp.MustCompile("[0-9]")

func getSpec(query string) (*QuerySpec, error) {
	u, err := url.ParseRequestURI(query)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	spec := QuerySpec{Query: q.Get("query")}
	if t := q.Get("start"); t != "" {
		start, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil, err
		}
		spec.Start = int(start)
	}
	if t := q.Get("end"); t != "" {
		end, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return nil, err
		}
		spec.End = int(end)
	}
	if t := q.Get("step"); t != "" {
		if t[len(t)-1] >= '0' && t[len(t)-1] <= '9' {
			t += "ns"
		}
		var err error
		spec.Step, err = time.ParseDuration(t)
		if err != nil {
			return nil, err
		}
	}
	return &spec, nil
}

// chop removes all items from promData where the value is less than
// start
func chop(promData *response.PromResponse, start int) {
	for j, result := range promData.Data.Result {
		// find index of first point
		first := -1
		for i, value := range result.Values {
			if int(value[0].(float64)) >= start {
				first = i
				break
			}
		}
		// chop
		if first != -1 {
			promData.Data.Result[j].Values = result.Values[first:]
		}
	}
}

// appendData merges head and tail, assuming that
// the step and time points are already compatible
// it mutates tail
func appendData(head, tail *response.PromResponse) (*response.PromResponse, error) {
	// some checks to see if they are mergable
	if head.Status != tail.Status {
		return nil, errors.New("head and tail status don't match")
	}
	if head.Data.ResultType != tail.Data.ResultType {
		return nil, errors.New("head and tail result type don't match")
	}
	// index newdata for appending into existing slice
	headmap := make(map[string][][]interface{}, len(head.Data.Result))
	for i, val := range head.Data.Result {
		headmap[val.Metric.Key()] = head.Data.Result[i].Values
	}
	for i, val := range tail.Data.Result {
		newMetric, found := headmap[val.Metric.Key()]
		if found {
			tail.Data.Result[i].Values = append(tail.Data.Result[i].Values, newMetric...)
		}
	}
	return tail, nil
}

// cacheCheck queries postgres for the closest match
func cacheCheck(db *sql.DB, spec *QuerySpec) (*response.PromResponse, *QuerySpec, error) {
	// Sorry, this is gross. Prototype
	if !*cache {
		return nil, nil, sql.ErrNoRows
	}
	// find candidate values. We pick the first value which overlaps the query
	// spec and is below the current value.
	query := `select tsrange, body from query_cache where query =$1 and step=$2 and tsrange &< $3 and tsrange && $3 and tsrange <> $3 order by tsrange desc limit 1`
	rng := fmt.Sprintf("[%d,%d)", spec.Start, spec.End)
	log.Printf("%+v %s %d %s", query, spec.Query, spec.Step, rng)
	row := db.QueryRow(query, spec.Query, spec.Step, rng)
	var body []byte
	var tsrange string
	if err := row.Scan(&tsrange, &body); err != nil {
		return nil, nil, err
	}
	var partialProm response.PromResponse
	if err := json.Unmarshal(body, &partialProm); err != nil {
		return nil, nil, err
	}
	// parse the tsrange [%d,%d)
	if len(tsrange) < 2 {
		return nil, nil, errors.New("Unable to parse range")
	}
	vals := strings.Split(tsrange[1:len(tsrange)-1], ",")
	if len(vals) != 2 {
		return nil, nil, errors.New("unable to parse range as 2 ints")
	}
	newSpec := *spec
	newSpec.Start, _ = strconv.Atoi(vals[0])
	newSpec.End, _ = strconv.Atoi(vals[1])
	return &partialProm, &newSpec, nil
}

func handleRangeRequest(db *sql.DB, query string) (*response.PromResponse, error) {
	var promData *response.PromResponse
	var buf bytes.Buffer
	// cache check
	spec, err := getSpec(query)
	if err != nil {
		return nil, err
	}
	if partialProm, partialSpec, err := cacheCheck(db, spec); err != nil {
		if err != sql.ErrNoRows {
			log.Printf("error checking cache, ignoring: %s", err)
		} else {
			log.Println("cache miss")
		}
	} else if partialSpec != nil {
		log.Println("cache hit")
		// got partial data. If for some reason we have a 100%
		// match, return it.
		if *spec == *partialSpec {
			return partialProm, nil
		}
		// partial cache hit. Lets' fetch the rest.
		// when making a request for [a,b) we can only
		// get a hit such that [a', b') where a <= a' <= b
		// so we need to chop a->a' and fetch b'->b
		chop(partialProm, spec.Start)
		fixupSpec := &QuerySpec{
			Step:  spec.Step,
			Start: partialSpec.End,
			End:   spec.End,
			Query: spec.Query,
		}
		// duplication of logic. fixme
		fixupQuery := fmt.Sprintf("/api/v1/query_range?query=%s&start=%d&end=%d&step=%d", url.QueryEscape(fixupSpec.Query), fixupSpec.Start, fixupSpec.End, fixupSpec.Step)
		fmt.Println(fixupQuery)
		fixupData, err := proxy(fixupQuery, nil)
		if err != nil {
			return nil, err
		}
		partialProm, err = appendData(fixupData, partialProm)
		if err != nil {
			// this is fine, fall back to full load
			log.Printf("couldn't merge data: %s", err)
		} else {
			// we don't store this in cache because it's not "canonical"
			return partialProm, nil
		}
	}

	if promData, err = proxy(query, &buf); err != nil {
		return nil, err
	}
	if promData.Status != "success" {
		return promData, nil
	}
	if promData.Data.ResultType != "matrix" {
		return promData, nil
	}
	// store in cache. Truncate to nearest minute if duration is > 5m
	if spec.End-spec.Start < 5*60 {
		return promData, nil
	}
	if *cache {
		// store large cache entry
		_, err = db.Exec("INSERT INTO query_cache(query, step, tsrange, body) VALUES ($1, $2, $3, $4)",
			spec.Query,
			spec.Step,
			fmt.Sprintf("[%d,%d)", spec.Start, spec.End),
			buf.Bytes(),
		)
		if err != nil {
			log.Printf("error inserting data: %s", err)
		}
	}
	return promData, nil
}

func proxy(uri string, cacheBuf *bytes.Buffer) (*response.PromResponse, error) {
	resp, err := http.Get(*backendProm + uri)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("non-200 status code")
	}
	var data response.PromResponse
	var rdr io.Reader = resp.Body
	if cacheBuf != nil {
		rdr = io.TeeReader(resp.Body, cacheBuf)
	}
	if err := json.NewDecoder(rdr).Decode(&data); err != nil {
		return nil, err
	}

	return &data, nil
}
