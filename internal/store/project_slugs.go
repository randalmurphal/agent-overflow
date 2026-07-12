package store

import (
	"database/sql"
	"fmt"
	"strings"
	"unicode"
)

const maxProjectSlugLength = 64

// ProjectSlug converts a project name into its filesystem-safe slug base.
// Uniqueness is assigned separately because it depends on persisted projects.
func ProjectSlug(name string) string {
	var b strings.Builder
	b.Grow(min(len(name), maxProjectSlugLength))
	lastWasDash := false
	for _, r := range name {
		r = unicode.ToLower(r)
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			if b.Len() < maxProjectSlugLength {
				b.WriteRune(r)
			}
			lastWasDash = false
			if b.Len() == maxProjectSlugLength {
				break
			}
			continue
		}
		if b.Len() > 0 && !lastWasDash && b.Len() < maxProjectSlugLength {
			b.WriteByte('-')
			lastWasDash = true
		}
		if b.Len() == maxProjectSlugLength {
			break
		}
	}

	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "project"
	}
	return slug
}

func nextProjectSlug(name string, taken func(string) (bool, error)) (string, error) {
	base := ProjectSlug(name)
	for sequence := 1; ; sequence++ {
		candidate := base
		if sequence > 1 {
			suffix := fmt.Sprintf("-%d", sequence)
			baseLimit := maxProjectSlugLength - len(suffix)
			trimmedBase := strings.TrimRight(base[:min(len(base), baseLimit)], "-")
			candidate = trimmedBase + suffix
		}

		exists, err := taken(candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
}

// backfillProjectSlugsFixup assigns deterministic slugs to existing rows,
// then installs the uniqueness constraint after every empty default is gone.
func backfillProjectSlugsFixup(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, name FROM projects ORDER BY created_at, id`)
	if err != nil {
		return fmt.Errorf("scan projects for slug backfill: %w", err)
	}
	type projectName struct {
		id   string
		name string
	}
	var projects []projectName
	for rows.Next() {
		var project projectName
		if err := rows.Scan(&project.id, &project.name); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan project for slug backfill: %w", err)
		}
		projects = append(projects, project)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate projects for slug backfill: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close project slug scan: %w", err)
	}

	assigned := make(map[string]struct{}, len(projects))
	for _, project := range projects {
		slug, err := nextProjectSlug(project.name, func(candidate string) (bool, error) {
			_, exists := assigned[candidate]
			return exists, nil
		})
		if err != nil {
			return err
		}
		assigned[slug] = struct{}{}
		if _, err := tx.Exec(`UPDATE projects SET slug = ? WHERE id = ?`, slug, project.id); err != nil {
			return fmt.Errorf("backfill project slug %s: %w", project.id, err)
		}
	}
	if _, err := tx.Exec(`CREATE UNIQUE INDEX idx_projects_slug ON projects(slug)`); err != nil {
		return fmt.Errorf("create unique project slug index: %w", err)
	}
	return nil
}
