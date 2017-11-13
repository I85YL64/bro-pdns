package main

import (
	"database/sql"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/kshvakov/clickhouse"
	"github.com/pkg/errors"
	clickhouse "github.com/roistat/go-clickhouse"
)

var chschema = []string{
	`
CREATE TABLE IF NOT EXISTS tuples (
    whatever Date DEFAULT '2000-01-01',
    query String,
    type String,
    answer String,
    ttl AggregateFunction(anyLast, UInt16),
    first AggregateFunction(min, DateTime),
    last AggregateFunction(max, DateTime),
    count AggregateFunction(sum, UInt64)
  ) ENGINE = AggregatingMergeTree(whatever, (query, type, answer), 8192);
`,

	`
CREATE TABLE IF NOT EXISTS individual (
    whatever Date DEFAULT '2000-01-01',
    which Enum8('Q'=0, 'A'=1),
    value String,
    first AggregateFunction(min, DateTime),
    last AggregateFunction(max, DateTime),
    count AggregateFunction(sum, UInt64)
  ) ENGINE = AggregatingMergeTree(whatever, (which, value), 8192);
`,
	`
CREATE TABLE IF NOT EXISTS filenames (
	day Date DEFAULT toDate(ts),
	ts DateTime DEFAULT now(),
	filename String,
	aggregation_time Float64,
	total_records UInt64,
	skipped_records UInt64,
	tuples UInt64,
	individual UInt64,
	store_time Float64,
	inserted UInt64,
	updated UInt64
  ) ENGINE = MergeTree(day, (filename), 8192);
`}

const tuples_temp_stmt = `
CREATE TABLE tuples_temp (
    query String,
    type String,
    answer String,
    ttl UInt16,
    first DateTime,
    last DateTime,
    count UInt64
) ENGINE = Log`

const individual_temp_stmt = `
CREATE TABLE individual_temp (
    which Enum8('Q'=0, 'A'=1),
    value String,
    first DateTime,
    last DateTime,
    count UInt64
) ENGINE = Log`

type CHStore struct {
	conn *sqlx.DB
	http *clickhouse.Conn
	host string
}

func NewCHStore(uri string) (Store, error) {
	url, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	conn, err := sqlx.Open("clickhouse", uri)
	if err != nil {
		return nil, err
	}
	err = conn.Ping()
	if err != nil {
		return nil, err
	}

	t := clickhouse.NewHttpTransport()
	t.Timeout = time.Second * 5
	http := clickhouse.NewConn(fmt.Sprintf("%s:8123/default", url.Hostname()), t)
	return &CHStore{
		conn: conn,
		http: http,
		host: url.Hostname(),
	}, nil
}

func (s *CHStore) Close() error {
	return s.Close()
}

func (s *CHStore) Exec(query string) error {
	q := clickhouse.NewQuery(query)
	err := q.Exec(s.http)
	return err
}

func (s *CHStore) Init() error {
	for _, stmt := range chschema {
		err := s.Exec(stmt)
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *CHStore) Clear() error {
	stmts := []string{"DELETE FROM filenames", "DELETE FROM individual", "DELETE FROM tuples"}
	for _, stmt := range stmts {
		err := s.Exec(stmt)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *CHStore) Begin() error {
	return fmt.Errorf("clickhouse doesn't support transactions")
}
func (s *CHStore) Commit() error {
	log.Printf("clickhouse doesn't support transactions")
	return nil
}

//DeleteOld Deletes records that haven't been seen in DAYS, returns the total records deleted
func (s *CHStore) DeleteOld(days int64) (int64, error) {
	return 0, fmt.Errorf("clickhouse doesn't support delete")
}

func (s *CHStore) SendJSON(table string, r io.Reader) error {
	timeout := time.Duration(60 * time.Second)
	client := http.Client{
		Timeout: timeout,
	}

	v := url.Values{}
	v.Set("query", fmt.Sprintf("INSERT INTO %s FORMAT JSONEachRow", table))
	qs := v.Encode()
	u := fmt.Sprintf("http://%s:8123?%s", s.host, qs)

	resp, err := client.Post(u, "application/json", r)
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("Clickhouse error: %s", body)
	}
	return err
}

func (s *CHStore) Update(ar aggregationResult) (UpdateResult, error) {
	var result UpdateResult
	start := time.Now()

	s.Exec("DROP TABLE tuples_temp")
	s.Exec("DROP TABLE individual_temp")

	err := s.Exec(tuples_temp_stmt)
	if err != nil {
		return result, errors.Wrap(err, "CHStore.Update failed to create temporary tuples table")
	}
	err = s.Exec(individual_temp_stmt)
	if err != nil {
		return result, errors.Wrap(err, "CHStore.Update failed to create temporary individual table")
	}
	defer func() {
		//s.conn.Exec("DROP TABLE tuples_temp")
		//s.conn.Exec("DROP TABLE individual_temp")
	}()

	err = s.SendJSON("tuples_temp", ar.TupleJSONReader(true))
	if err != nil {
		return result, errors.Wrap(err, "CHStore.Update tuples failed")
	}

	err = s.Exec(`INSERT INTO tuples (query, type, answer, ttl, first, last, count) SELECT query, type, answer, anyLastState(ttl), minState(first), maxState(last), sumState(count) from tuples_temp group by query, type, answer`)
	if err != nil {
		return result, errors.Wrap(err, "CHStore.Update failed to insert into tuples")
	}

	err = s.SendJSON("individual_temp", ar.IndividualJSONReader(true))
	if err != nil {
		return result, errors.Wrap(err, "CHStore.Update individual failed")
	}

	err = s.Exec(`INSERT INTO individual (which, value, first, last, count) SELECT which, value, minState(first), maxState(last), sumState(count) from individual_temp group by which, value`)
	if err != nil {
		return result, errors.Wrap(err, "CHStore.Update failed to insert into individual")
	}
	result.Duration = time.Since(start)
	return result, nil
}

func (s *CHStore) IsLogIndexed(filename string) (bool, error) {
	var fn string
	err := s.conn.QueryRow("SELECT filename FROM filenames WHERE filename=?", filename).Scan(&fn)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}
func (s *CHStore) SetLogIndexed(filename string, ar aggregationResult, ur UpdateResult) error {
	tx, _ := s.conn.Begin()
	q := `INSERT INTO filenames (filename,
	      aggregation_time, total_records, skipped_records, tuples, individual,
	      store_time, inserted, updated)
	      VALUES (?,?,?,?,?,?,?,?,?)`
	_, err := tx.Exec(q, filename,
		ar.Duration.Seconds(), uint64(ar.TotalRecords), uint64(ar.SkippedRecords), len(ar.Tuples), len(ar.Individual),
		ur.Duration.Seconds(), uint64(ur.Inserted), uint64(ur.Updated))
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CHStore) FindQueryTuples(query string) (tupleResults, error) {
	tr := []tupleResult{}
	query = Reverse(query)
	err := s.conn.Select(&tr, "SELECT * FROM tuples WHERE query = ?", query)
	reverseQuery(tr)
	return tr, err
}
func (s *CHStore) FindTuples(query string) (tupleResults, error) {
	tr := []tupleResult{}
	rquery := Reverse(query)
	err := s.conn.Select(&tr, "SELECT query, type, answer, minMerge(first) as first, maxMerge(last) as last, sumMerge(count) as count from tuples WHERE query = ? OR answer = ? group by query, type, answer ORDER BY query, answer", rquery, query)
	reverseQuery(tr)

	return tr, err
}
func (s *CHStore) LikeTuples(query string) (tupleResults, error) {
	tr := []tupleResult{}
	rquery := Reverse(query)
	err := s.conn.Select(&tr, "SELECT query, type, answer, minMerge(first) as first, maxMerge(last) as last, sumMerge(count) as count from tuples WHERE query like ? OR answer like ? group by query, type, answer ORDER BY query, answer", rquery+"%", query+"%")
	reverseQuery(tr)
	return tr, err
}
func (s *CHStore) FindIndividual(value string) (individualResults, error) {
	rvalue := Reverse(value)
	tr := []individualResult{}
	err := s.conn.Select(&tr, `SELECT which, value, minMerge(first) as first, maxMerge(last) as last, sumMerge(count) as count from individual WHERE (which='A' AND value = ?) OR (which='Q' AND value = ?) group by which, value ORDER BY value`, value, rvalue)
	reverseValue(tr)
	return tr, err
}

func (s *CHStore) LikeIndividual(value string) (individualResults, error) {
	rvalue := Reverse(value)
	tr := []individualResult{}
	err := s.conn.Select(&tr, `SELECT which, value, minMerge(first) as first, maxMerge(last) as last, sumMerge(count) as count from individual WHERE (which='A' AND value like ?) OR (which='Q' AND value like ?) group by which, value ORDER BY value`, value+"%", rvalue+"%")
	reverseValue(tr)
	return tr, err
}