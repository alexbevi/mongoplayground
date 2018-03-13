package main

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/dgraph-io/badger"
	cfg "github.com/feliixx/mgodatagen/config"
	"github.com/feliixx/mgodatagen/generators"
	"github.com/globalsign/mgo"
	"github.com/globalsign/mgo/bson"
)

var (
	templates = template.Must(template.ParseFiles("playground.html"))
	homePage  []byte
)

const (
	mgodatagenMode = byte(0)
	jsonMode       = byte(1)
	badgerDir      = "storage"
	// interval between two database cleanup
	cleanupInterval = 60 * time.Minute
	// if a database is not used within the last
	// expireInterval, it is removed in the next cleanup
	expireInterval = 60 * time.Minute
	// max number of collection to create at once
	maxCollNb = 10
	// max number of documents in a collection
	maxDoc = 100
	// max size of a collection
	maxBytes = 100 * 1024
	// template configuration
	templateConfig = `[
  {
    "collection": "collection",
    "count": 10,
    "content": {
		"k": {
			"type": "int",
			"minInt": 0, 
			"maxInt": 10
		}
	}
  }
]`
	// template query
	templateQuery = "db.collection.find()"
	// NoDocFound error message when no docs match the query
	NoDocFound = "no document found"
)

func getHomeBytes(version []byte) ([]byte, error) {
	var buf bytes.Buffer
	p := &page{
		ModeJSON:     false,
		Config:       []byte(templateConfig),
		Query:        []byte(templateQuery),
		MongoVersion: version,
	}
	err := templates.Execute(&buf, p)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type page struct {
	ModeJSON bool
	// configuration used to generate the sample database
	Config []byte
	// query to run against the collection / database
	Query []byte
	// mongodb version
	MongoVersion []byte
}

func computeID(mode, query, config []byte) []byte {
	e := sha256.New()
	e.Write(mode)
	e.Write(query)
	e.Write(config)
	sum := e.Sum(nil)
	b := make([]byte, base64.URLEncoding.EncodedLen(len(sum)))
	base64.URLEncoding.Encode(b, sum)
	return b[:11]
}

func connect() (*mgo.Session, []byte, error) {
	s, err := mgo.Dial("mongodb://")
	if err != nil {
		return nil, nil, fmt.Errorf("fail to connect to mongodb: %v", err)
	}
	info, _ := s.BuildInfo()
	return s, []byte(info.Version), nil
}

func getMode(mode []byte) byte {
	if bytes.Equal(mode, []byte("json")) {
		return jsonMode
	}
	return mgodatagenMode
}

type server struct {
	mux            *http.ServeMux
	session        *mgo.Session
	storage        *badger.DB
	activeDB       sync.Map
	mongodbVersion []byte
}

func newServer() (*server, error) {
	session, version, err := connect()
	if err != nil {
		return nil, err
	}

	opts := badger.DefaultOptions
	opts.Dir = badgerDir
	opts.ValueDir = badgerDir
	db, err := badger.Open(opts)
	if err != nil {
		return nil, err
	}

	s := &server{
		mux:            http.DefaultServeMux,
		session:        session,
		storage:        db,
		activeDB:       sync.Map{},
		mongodbVersion: version,
	}

	go func(s *server) {
		for range time.Tick(cleanupInterval) {
			s.removeExpiredDB()
		}
	}(s)

	h, err := getHomeBytes(version)
	if err != nil {
		return nil, err
	}
	homePage = h

	s.mux.HandleFunc("/", s.newPageHandler)
	s.mux.HandleFunc("/p/", s.viewHandler)
	s.mux.HandleFunc("/run/", s.runHandler)
	s.mux.HandleFunc("/save/", s.saveHandler)
	s.mux.Handle("/static/", s.staticHandler())

	return s, nil
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// view a saved playground page identified by his ID
// a playground url currently looks like
// url/p/adfadadH7ha
func (s *server) viewHandler(w http.ResponseWriter, r *http.Request) {
	url := r.URL.Path
	i := strings.LastIndex(url, "/p/")
	ID := []byte(url[i+3:])
	p := &page{
		MongoVersion: s.mongodbVersion,
	}
	err := s.storage.View(func(txn *badger.Txn) error {
		item, err := txn.Get(ID)
		if err != nil {
			return err
		}
		val, err := item.Value()
		if err != nil {
			return err
		}
		split := binary.LittleEndian.Uint32(val[0:4])
		if val[4] == jsonMode {
			p.ModeJSON = true
		}
		p.Config = val[5:split]
		p.Query = val[split:]
		return nil
	})
	if err != nil {
		http.Redirect(w, r, "/", http.StatusNotFound)
		return
	}
	templates.Execute(w, p)
}

// run a query and return the results as plain text
func (s *server) runHandler(w http.ResponseWriter, r *http.Request) {
	mode, config, query := []byte(r.FormValue("mode")), []byte(r.FormValue("config")), []byte(r.FormValue("query"))
	res, err := s.generateSample(getMode(mode), config, query)
	if err != nil {
		w.Write([]byte(err.Error()))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write(res)
}

// save the playground and return the playground ID
func (s *server) saveHandler(w http.ResponseWriter, r *http.Request) {
	mode, config, query := []byte(r.FormValue("mode")), []byte(r.FormValue("config")), []byte(r.FormValue("query"))

	ID := computeID(mode, config, query)
	s.storage.Update(func(txn *badger.Txn) error {
		v := make([]byte, 5+len(config)+len(query))

		split := len(config) + 5
		binary.LittleEndian.PutUint32(v[0:4], uint32(split))
		v[4] = getMode(mode)
		copy(v[5:split], config)
		copy(v[split:], query)

		return txn.Set(ID, v)
	})
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "%sp/%s", r.Referer(), ID)
}

// return a playground with the default configuration
func (s *server) newPageHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(homePage)
}

// serve static ressources (css/js/html)
func (s *server) staticHandler() http.Handler {
	return http.StripPrefix("/static/", http.FileServer(http.Dir("./static")))
}

func (s *server) generateSample(mode byte, config, query []byte) ([]byte, error) {

	session := s.session.Copy()
	defer session.Close()

	DBHash := fmt.Sprintf("%x", md5.Sum(append(config, mode)))
	db := session.DB(DBHash)

	_, exists := s.activeDB.LoadOrStore(DBHash, time.Now().Unix())
	if !exists {
		db.DropDatabase()
		switch mode {
		case mgodatagenMode:
			listColl, err := cfg.ParseConfig(config, true)
			if err != nil {
				return nil, fmt.Errorf("fail to parse configuration: %v", err)
			}
			if len(listColl) > maxCollNb {
				return nil, fmt.Errorf("Max number of collections to create is %d, but found %d collections", maxCollNb, len(listColl))
			}
			for _, c := range listColl {
				coll := createCollection(db, c.Name)
				err := fillCollection(c, coll)
				if err != nil {
					return nil, fmt.Errorf("fail to create DB: %v", err)
				}
			}
		case jsonMode:
			var docs []bson.M
			err := bson.UnmarshalJSON(config, &docs)
			if err != nil {
				return nil, fmt.Errorf("json: fail to parse content, expected an array of JSON documents")
			}
			coll := createCollection(db, "collection")
			bulk := coll.Bulk()
			bulk.Unordered()
			if len(docs) > 0 {
				for i, doc := range docs {
					if _, hasID := doc["_id"]; !hasID {
						doc["_id"] = bson.ObjectId(objectIDBytes(int32(i)))
					}
					bulk.Insert(doc)
				}
			}
			_, err = bulk.Run()
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("invalid mode")
		}
	}
	return runQuery(db, query)
}

func createCollection(db *mgo.Database, name string) *mgo.Collection {
	info := &mgo.CollectionInfo{
		Capped:   true,
		MaxDocs:  maxDoc,
		MaxBytes: maxBytes,
	}
	c := db.C(name)
	c.Create(info)
	return c
}

func fillCollection(c cfg.Collection, coll *mgo.Collection) error {
	ci := &generators.CollInfo{
		Encoder:    generators.NewEncoder(4, 1),
		Version:    []int{3, 6},
		ShortNames: false,
		Count:      c.Count,
	}
	if ci.Count > maxDoc || ci.Count <= 0 {
		ci.Count = maxDoc
	}
	g, err := ci.CreateGenerator(c.Content)
	if err != nil {
		return fmt.Errorf("fail to create collection %s: %v", c.Name, err)
	}
	// if the config doesn't contain an _id generator, add a seeded one to generate
	// always the same sequence of ObjectId
	if _, hasID := c.Content["_id"]; !hasID {
		g.Generators = append(g.Generators, &SeededObjectIDGenerator{
			idx: 0,
			EmptyGenerator: generators.EmptyGenerator{
				K:              append([]byte("_id"), byte(0)),
				NullPercentage: 0,
				T:              bson.ElementObjectId,
				Out:            ci.Encoder,
			},
		})
	}

	bulk := coll.Bulk()
	bulk.Unordered()

	for i := 0; i < ci.Count; i++ {
		g.Value()
		b := make([]byte, len(ci.Encoder.Data))
		copy(b, ci.Encoder.Data)
		bulk.Insert(bson.Raw{Data: b})
	}
	_, err = bulk.Run()
	return err
}

// remove db not used within the last expireInterval
func (s *server) removeExpiredDB() {
	now := time.Now()
	session := s.session.Copy()
	defer session.Close()
	s.activeDB.Range(func(k, v interface{}) bool {
		if now.Sub(time.Unix(v.(int64), 0)) > expireInterval {
			s.activeDB.Delete(k)
			session.DB(k.(string)).DropDatabase()
		}
		return true
	})
}

func runQuery(db *mgo.Database, query []byte) ([]byte, error) {
	p := bytes.SplitN(query, []byte("."), 3)
	if len(p) != 3 {
		return nil, fmt.Errorf("invalid query: \nmust match db.coll.find(...) or db.coll.aggregate(...)")
	}

	if bytes.Equal(p[2], []byte("find()")) {
		p[2] = []byte("find({})")
	}
	start := bytes.Index(p[2], []byte("("))
	end := bytes.LastIndex(p[2], []byte(")"))

	collection := db.C(string(p[1]))
	var docs []interface{}

	switch string(p[2][:start]) {
	case "find":
		var query bson.M
		err := bson.UnmarshalJSON(p[2][start+1:end], &query)
		if err != nil {
			return nil, fmt.Errorf("Find query failed: %v", err)
		}
		err = collection.Find(query).All(&docs)
		if err != nil {
			return nil, fmt.Errorf("Find query failed: %v", err)
		}
	case "aggregate":
		var pipeline []bson.M
		err := bson.UnmarshalJSON(p[2][start+1:end], &pipeline)
		if err != nil {
			return nil, fmt.Errorf("Aggregate query failed: %v", err)
		}
		err = collection.Pipe(pipeline).All(&docs)
		if err != nil {
			return nil, fmt.Errorf("Aggregate query failed: %v", err)
		}
	default:
		// this should never happend as invalid queries are filtered from front-end size
		return nil, fmt.Errorf("invalid method: %s", p[2][:start])
	}
	if len(docs) == 0 {
		return []byte(NoDocFound), nil
	}
	return bson.MarshalJSON(docs)
}