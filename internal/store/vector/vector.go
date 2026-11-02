package vector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/sarpaydemir/melinda/internal/store/pg"
)

// searchSQL ranks the collection's rows that pass the filter condition (%s) by cosine distance.
// OFFSET 0 keeps the subquery from being flattened, so no vector index can order it and the ranking is exact.
const searchSQL = `SELECT id, document, cmetadata, distance
FROM (
	SELECT id, document, cmetadata, embedding <=> $1 AS distance
	FROM langchain_pg_embedding
	WHERE collection_id = $2 %s
	OFFSET 0
) AS candidates
ORDER BY distance
LIMIT $3`

// Doc is one row to add.
type Doc struct {
	Document  string
	Embedding []float32
	Metadata  json.RawMessage // stored as given; empty stores {}
}

// Hit is one search result.
type Hit struct {
	ID       string
	Document *string         // nil when the row has none
	Metadata json.RawMessage // nil when the row has none
	Distance float64         // cosine distance; NaN for a zero vector
}

// DimensionError reports a vector whose length is not pg.EmbeddingDims.
type DimensionError struct{ Len int }

func (e *DimensionError) Error() string {
	return fmt.Sprintf("vector has %d dimensions, want %d", e.Len, pg.EmbeddingDims)
}

// Store keeps documents and their embeddings in named collections.
type Store struct {
	db  *pg.DB
	mu  sync.Mutex
	ids map[string]string // collection name to uuid
}

// New returns a Store on db.
func New(db *pg.DB) *Store { return &Store{db: db, ids: map[string]string{}} }

// Add stores docs in collection in one transaction.
func (s *Store) Add(ctx context.Context, collection string, docs []Doc) error {
	for _, d := range docs {
		if err := checkLen(d.Embedding); err != nil {
			return err
		}
	}
	id, err := s.collectionID(ctx, collection)
	if err != nil {
		return err
	}
	return s.db.WithTx(ctx, func(tx pgx.Tx) error {
		for _, d := range docs {
			metadata := d.Metadata
			if len(metadata) == 0 {
				metadata = json.RawMessage(`{}`)
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO langchain_pg_embedding (id, collection_id, embedding, document, cmetadata)
				VALUES (gen_random_uuid()::text, $1, $2, $3, $4)`,
				id, vectorText(d.Embedding), d.Document, metadata); err != nil {
				return err
			}
		}
		return nil
	})
}

// Search returns up to limit rows of collection nearest to vec by cosine distance, nearest first, among those
// whose metadata holds every key of filter with that string value.
func (s *Store) Search(ctx context.Context, collection string, vec []float32, filter map[string]string, limit int) ([]Hit, error) {
	if err := checkLen(vec); err != nil {
		return nil, err
	}
	id, err := s.collectionID(ctx, collection)
	if err != nil {
		return nil, err
	}
	sql, args := searchQuery(id, vec, filter, limit)
	rows, _ := s.db.Query(ctx, sql, args...)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Hit])
}

func searchQuery(collectionID string, vec []float32, filter map[string]string, limit int) (string, []any) {
	cond, args := containment(filter, []any{vectorText(vec), collectionID, limit})
	return fmt.Sprintf(searchSQL, cond), args
}

// Delete removes the rows of collection whose metadata holds every key of filter with that string value, and
// returns how many it removed. An empty filter is an error; DeleteAll empties a collection.
func (s *Store) Delete(ctx context.Context, collection string, filter map[string]string) (int64, error) {
	if len(filter) == 0 {
		return 0, errors.New("delete needs a metadata filter")
	}
	return s.delete(ctx, collection, filter)
}

// DeleteAll removes every row of collection and returns how many it removed.
func (s *Store) DeleteAll(ctx context.Context, collection string) (int64, error) {
	return s.delete(ctx, collection, nil)
}

func (s *Store) delete(ctx context.Context, collection string, filter map[string]string) (int64, error) {
	id, err := s.collectionID(ctx, collection)
	if err != nil {
		return 0, err
	}
	cond, args := containment(filter, []any{id})
	tag, err := s.db.Exec(ctx, "DELETE FROM langchain_pg_embedding WHERE collection_id = $1 "+cond, args...)
	return tag.RowsAffected(), err
}

// containment appends filter to args as one JSON object and returns the condition matching it; an empty filter
// adds nothing.
func containment(filter map[string]string, args []any) (string, []any) {
	if len(filter) == 0 {
		return "", args
	}
	b, _ := json.Marshal(filter)
	args = append(args, b)
	return fmt.Sprintf("AND cmetadata::jsonb @> $%d::jsonb", len(args)), args
}

// collectionID returns the uuid of the named collection, creating the collection on first use.
func (s *Store) collectionID(ctx context.Context, name string) (string, error) {
	s.mu.Lock()
	id, ok := s.ids[name]
	s.mu.Unlock()
	if ok {
		return id, nil
	}
	if _, err := s.db.Exec(ctx, `
		INSERT INTO langchain_pg_collection (uuid, name, cmetadata) VALUES (gen_random_uuid(), $1, NULL)
		ON CONFLICT (name) DO NOTHING`, name); err != nil {
		return "", fmt.Errorf("create collection %s: %w", name, err)
	}
	rows, _ := s.db.Query(ctx, "SELECT uuid::text FROM langchain_pg_collection WHERE name = $1", name)
	id, err := pgx.CollectExactlyOneRow(rows, pgx.RowTo[string])
	if err != nil {
		return "", fmt.Errorf("read collection %s: %w", name, err)
	}
	s.mu.Lock()
	s.ids[name] = id
	s.mu.Unlock()
	return id, nil
}

func checkLen(vec []float32) error {
	if len(vec) != pg.EmbeddingDims {
		return &DimensionError{Len: len(vec)}
	}
	return nil
}

// vectorText renders vec in pgvector's text form; each component reads back as the same float32.
func vectorText(vec []float32) string {
	b := make([]byte, 0, 16*len(vec))
	b = append(b, '[')
	for i, f := range vec {
		if i > 0 {
			b = append(b, ',')
		}
		b = strconv.AppendFloat(b, float64(f), 'g', -1, 32)
	}
	return string(append(b, ']'))
}
