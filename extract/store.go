// pgx-backed Store: the read side of the database for unit builders and
// the runner.

package extract

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolStore implements Store over a pgx connection pool.
type PoolStore struct{ Pool *pgxpool.Pool }

// NewStore wraps a pool in the Store interface.
func NewStore(pool *pgxpool.Pool) *PoolStore { return &PoolStore{Pool: pool} }

// LatestDocuments returns the newest revision per URL for one source —
// the document set an extraction run covers.
func (s *PoolStore) LatestDocuments(ctx context.Context, sourceID string) ([]Document, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT rd.id, rd.source_id, rd.title, rd.url, rd.fetched_at,
		       COALESCE(rd.revision, ''), COALESCE(rd.content_text, '')
		FROM raw_documents rd
		WHERE rd.source_id = $1
		  AND rd.fetched_at = (
		      SELECT MAX(r2.fetched_at) FROM raw_documents r2
		      WHERE r2.source_id = rd.source_id AND r2.url = rd.url)
		ORDER BY rd.title`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("query latest documents: %w", err)
	}
	defer rows.Close()

	documents := []Document{}
	for rows.Next() {
		var d Document
		if err := rows.Scan(&d.ID, &d.SourceID, &d.Title, &d.URL, &d.FetchedAt, &d.Revision, &d.ContentText); err != nil {
			return nil, fmt.Errorf("scan document: %w", err)
		}
		documents = append(documents, d)
	}
	return documents, rows.Err()
}

// Sources returns the registry keyed by id.
func (s *PoolStore) Sources(ctx context.Context) (map[string]Source, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, tier, title, COALESCE(citation_form, ''), locator_scheme
		FROM sources`)
	if err != nil {
		return nil, fmt.Errorf("query sources: %w", err)
	}
	defer rows.Close()

	sources := map[string]Source{}
	for rows.Next() {
		var src Source
		if err := rows.Scan(&src.ID, &src.Tier, &src.Title, &src.CitationForm, &src.LocatorScheme); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		sources[src.ID] = src
	}
	return sources, rows.Err()
}

// KnownEntityIDs returns every entity id in the graph.
func (s *PoolStore) KnownEntityIDs(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id FROM entities ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("query entities: %w", err)
	}
	return scanIDs(rows)
}

// RelationshipTypes returns the allowed rel_type vocabulary.
func (s *PoolStore) RelationshipTypes(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT rel_type FROM relationship_types ORDER BY rel_type`)
	if err != nil {
		return nil, fmt.Errorf("query relationship types: %w", err)
	}
	return scanIDs(rows)
}

// RelationshipTypeAliases maps surface spellings to canonical types.
func (s *PoolStore) RelationshipTypeAliases(ctx context.Context) (map[string]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT alias, rel_type FROM relationship_type_aliases`)
	if err != nil {
		return nil, fmt.Errorf("query relationship aliases: %w", err)
	}
	defer rows.Close()

	aliases := map[string]string{}
	for rows.Next() {
		var alias, relType string
		if err := rows.Scan(&alias, &relType); err != nil {
			return nil, fmt.Errorf("scan alias: %w", err)
		}
		aliases[alias] = relType
	}
	return aliases, rows.Err()
}

func scanIDs(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
