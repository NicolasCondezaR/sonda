package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// A Project groups the services of one system: a monorepo, a side project,
// whatever is being worked on today.
//
// The grouping is not filing for its own sake. It carries the two things that
// are shared across a system's services and were duplicated when they were a
// flat list: one protobuf descriptor set for all of them, and one answer to
// "are these ports open right now". Only the active project listens, so two
// projects can claim the same port without colliding.
type Project struct {
	ID       int64
	Name     string
	Active   bool
	Services []Service

	// DescriptorSet is stored, not referenced. A path breaks the moment the
	// database is copied to another machine, and copying the database between
	// machines is the whole point of a single-file store.
	DescriptorSet     []byte
	DescriptorName    string
	DescriptorUpdated time.Time
}

type Service struct {
	ID         int64
	ProjectID  int64
	Name       string
	Listen     string
	Upstream   string
	Protocol   string
	Reflection bool
	Position   int
}

var (
	ErrProjectNotFound = errors.New("no project with that id")
	ErrDuplicateName   = errors.New("a project with that name already exists")
)

const projectSchema = `
CREATE TABLE IF NOT EXISTS projects (
	id                 INTEGER PRIMARY KEY AUTOINCREMENT,
	name               TEXT    NOT NULL UNIQUE,
	active             INTEGER NOT NULL DEFAULT 0,
	descriptor_set     BLOB,
	descriptor_name    TEXT    NOT NULL DEFAULT '',
	descriptor_updated INTEGER NOT NULL DEFAULT 0,
	created_at         INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS services (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name       TEXT    NOT NULL,
	listen     TEXT    NOT NULL,
	upstream   TEXT    NOT NULL,
	protocol   TEXT    NOT NULL,
	reflection INTEGER NOT NULL DEFAULT 1,
	position   INTEGER NOT NULL DEFAULT 0,
	UNIQUE(project_id, name)
);
CREATE INDEX IF NOT EXISTS services_project_idx ON services(project_id, position);
`

func (s *Store) Projects(ctx context.Context) ([]Project, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, active, descriptor_name, descriptor_updated,
		       LENGTH(COALESCE(descriptor_set, ''))
		FROM projects ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	out := []Project{}
	for rows.Next() {
		var (
			p            Project
			active, size int
			updated      int64
		)
		if err := rows.Scan(&p.ID, &p.Name, &active, &p.DescriptorName, &updated, &size); err != nil {
			return nil, err
		}
		p.Active = active != 0
		if updated > 0 {
			p.DescriptorUpdated = time.UnixMicro(updated).UTC()
		}
		// The blob itself is left behind: a listing does not need megabytes of
		// descriptors, only whether one is there.
		if size > 0 {
			p.DescriptorSet = []byte{}
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range out {
		if out[i].Services, err = s.services(ctx, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Store) services(ctx context.Context, projectID int64) ([]Service, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, project_id, name, listen, upstream, protocol, reflection, position
		FROM services WHERE project_id = ? ORDER BY position, id`, projectID)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()

	out := []Service{}
	for rows.Next() {
		var (
			svc        Service
			reflection int
		)
		if err := rows.Scan(&svc.ID, &svc.ProjectID, &svc.Name, &svc.Listen,
			&svc.Upstream, &svc.Protocol, &reflection, &svc.Position); err != nil {
			return nil, err
		}
		svc.Reflection = reflection != 0
		out = append(out, svc)
	}
	return out, rows.Err()
}

// ActiveProject is the one whose ports are open. Nil when nothing is active,
// which is a legitimate state: a fresh install has no project yet.
func (s *Store) ActiveProject(ctx context.Context) (*Project, error) {
	var (
		p       Project
		updated int64
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, descriptor_set, descriptor_name, descriptor_updated
		FROM projects WHERE active = 1 LIMIT 1`,
	).Scan(&p.ID, &p.Name, &p.DescriptorSet, &p.DescriptorName, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Active = true
	if updated > 0 {
		p.DescriptorUpdated = time.UnixMicro(updated).UTC()
	}
	if p.Services, err = s.services(ctx, p.ID); err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Store) CreateProject(ctx context.Context, name string) (*Project, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("a project needs a name")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO projects (name, created_at) VALUES (?, ?)`, name, time.Now().UnixMicro())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, ErrDuplicateName
		}
		return nil, fmt.Errorf("create project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &Project{ID: id, Name: name, Services: []Service{}}, nil
}

func (s *Store) RenameProject(ctx context.Context, id int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a project needs a name")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE projects SET name = ? WHERE id = ?`, name, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return ErrDuplicateName
		}
		return err
	}
	return affected(res, ErrProjectNotFound)
}

func (s *Store) DeleteProject(ctx context.Context, id int64) error {
	// Services go with it through the foreign key, but the pragma is off by
	// default in SQLite, so the delete is explicit rather than trusted.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM services WHERE project_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if err := affected(res, ErrProjectNotFound); err != nil {
		return err
	}
	return tx.Commit()
}

// ActivateProject makes one project the listening one. Exactly one at a time:
// two projects with open ports would fight over them, and the whole reason to
// group services is that a project is the unit you switch between.
func (s *Store) ActivateProject(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM projects WHERE id = ?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return ErrProjectNotFound
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET active = 0 WHERE active = 1`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE projects SET active = 1 WHERE id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// DeactivateProjects closes everything without deleting anything.
func (s *Store) DeactivateProjects(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE projects SET active = 0`)
	return err
}

func (s *Store) SetDescriptorSet(ctx context.Context, id int64, name string, blob []byte) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects SET descriptor_set = ?, descriptor_name = ?, descriptor_updated = ?
		WHERE id = ?`, blob, name, time.Now().UnixMicro(), id)
	if err != nil {
		return err
	}
	return affected(res, ErrProjectNotFound)
}

func (s *Store) DescriptorSet(ctx context.Context, id int64) ([]byte, error) {
	var blob []byte
	err := s.db.QueryRowContext(ctx, `SELECT descriptor_set FROM projects WHERE id = ?`, id).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProjectNotFound
	}
	return blob, err
}

func (s *Store) SaveService(ctx context.Context, svc Service) (int64, error) {
	if err := ValidateService(svc); err != nil {
		return 0, err
	}
	if svc.ID == 0 {
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO services (project_id, name, listen, upstream, protocol, reflection, position)
			VALUES (?,?,?,?,?,?,?)`,
			svc.ProjectID, svc.Name, svc.Listen, svc.Upstream, svc.Protocol,
			boolToInt(svc.Reflection), svc.Position)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return 0, fmt.Errorf("this project already has a service called %q", svc.Name)
			}
			return 0, err
		}
		return res.LastInsertId()
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE services SET name = ?, listen = ?, upstream = ?, protocol = ?,
		       reflection = ?, position = ?
		WHERE id = ?`,
		svc.Name, svc.Listen, svc.Upstream, svc.Protocol,
		boolToInt(svc.Reflection), svc.Position, svc.ID)
	if err != nil {
		return 0, err
	}
	return svc.ID, affected(res, errors.New("no service with that id"))
}

func (s *Store) DeleteService(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM services WHERE id = ?`, id)
	if err != nil {
		return err
	}
	return affected(res, errors.New("no service with that id"))
}

// ValidateService rejects what would fail at listen time, so a bad entry is a
// message in the form rather than a port that silently never opened.
func ValidateService(svc Service) error {
	if strings.TrimSpace(svc.Name) == "" {
		return errors.New("the service needs a name")
	}
	if svc.Protocol != "http" && svc.Protocol != "grpc" {
		return fmt.Errorf("protocol %q is not supported, use http or grpc", svc.Protocol)
	}
	if _, _, err := splitHostPort(svc.Listen); err != nil {
		return fmt.Errorf("listen address %q: %w", svc.Listen, err)
	}

	u, err := url.Parse(svc.Upstream)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("upstream %q must look like http://host:port", svc.Upstream)
	}
	if svc.Listen == u.Host {
		return errors.New("the listen address and the upstream are the same, which would make the service call itself")
	}
	return nil
}

func splitHostPort(addr string) (host, port string, err error) {
	idx := strings.LastIndex(addr, ":")
	if idx <= 0 || idx == len(addr)-1 {
		return "", "", errors.New("expected host:port")
	}
	return addr[:idx], addr[idx+1:], nil
}

func affected(res sql.Result, notFound error) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound
	}
	return nil
}
