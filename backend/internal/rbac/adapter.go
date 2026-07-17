package rbac

import (
	"context"
	"fmt"
	"strings"

	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type PgxAdapter struct {
	pool *pgxpool.Pool
}

func NewPgxAdapter(pool *pgxpool.Pool) *PgxAdapter {
	return &PgxAdapter{pool: pool}
}

func (a *PgxAdapter) LoadPolicy(m model.Model) error {
	ctx := context.Background()
	rows, err := a.pool.Query(ctx, `SELECT p_type, v0, v1, v2, v3, v4, v5 FROM casbin_rule`)
	if err != nil {
		return fmt.Errorf("loading casbin rules: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var pType, v0, v1, v2, v3, v4, v5 string
		if err := rows.Scan(&pType, &v0, &v1, &v2, &v3, &v4, &v5); err != nil {
			return fmt.Errorf("scanning casbin rule: %w", err)
		}
		line := buildLine(pType, v0, v1, v2, v3, v4, v5)
		if err := persist.LoadPolicyLine(line, m); err != nil {
			return fmt.Errorf("loading policy line %q: %w", line, err)
		}
	}
	return rows.Err()
}

func (a *PgxAdapter) SavePolicy(m model.Model) error {
	ctx := context.Background()
	tx, err := a.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM casbin_rule`); err != nil {
		return fmt.Errorf("deleting casbin rules: %w", err)
	}

	for pType, ast := range m["p"] {
		for _, rule := range ast.Policy {
			if err := a.insertRule(ctx, tx, pType, rule); err != nil {
				return err
			}
		}
	}
	for pType, ast := range m["g"] {
		for _, rule := range ast.Policy {
			if err := a.insertRule(ctx, tx, pType, rule); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

func (a *PgxAdapter) insertRule(ctx context.Context, e execer, pType string, rule []string) error {
	vals := padValues(rule, 6)
	_, err := e.Exec(ctx,
		`INSERT INTO casbin_rule (p_type, v0, v1, v2, v3, v4, v5) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		pType, vals[0], vals[1], vals[2], vals[3], vals[4], vals[5],
	)
	return err
}

func (a *PgxAdapter) AddPolicy(sec string, pType string, rule []string) error {
	vals := padValues(rule, 6)
	_, err := a.pool.Exec(context.Background(),
		`INSERT INTO casbin_rule (p_type, v0, v1, v2, v3, v4, v5) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		pType, vals[0], vals[1], vals[2], vals[3], vals[4], vals[5],
	)
	return err
}

func (a *PgxAdapter) RemovePolicy(sec string, pType string, rule []string) error {
	vals := padValues(rule, 6)
	_, err := a.pool.Exec(context.Background(),
		`DELETE FROM casbin_rule WHERE p_type=$1 AND v0=$2 AND v1=$3 AND v2=$4 AND v3=$5 AND v4=$6 AND v5=$7`,
		pType, vals[0], vals[1], vals[2], vals[3], vals[4], vals[5],
	)
	return err
}

func (a *PgxAdapter) RemoveFilteredPolicy(sec string, pType string, fieldIndex int, fieldValues ...string) error {
	ctx := context.Background()
	query := fmt.Sprintf(`DELETE FROM casbin_rule WHERE p_type='%s'`, pType)
	for i := 0; i < len(fieldValues); i++ {
		if fieldValues[i] != "" {
			query += fmt.Sprintf(" AND v%d='%s'", fieldIndex+i, fieldValues[i])
		}
	}
	_, err := a.pool.Exec(ctx, query)
	return err
}

func (a *PgxAdapter) AddPolicies(sec string, pType string, rules [][]string) error {
	for _, rule := range rules {
		if err := a.AddPolicy(sec, pType, rule); err != nil {
			return err
		}
	}
	return nil
}

func (a *PgxAdapter) RemovePolicies(sec string, pType string, rules [][]string) error {
	for _, rule := range rules {
		if err := a.RemovePolicy(sec, pType, rule); err != nil {
			return err
		}
	}
	return nil
}

func padValues(rule []string, n int) []string {
	result := make([]string, n)
	for i := 0; i < n; i++ {
		if i < len(rule) {
			result[i] = rule[i]
		} else {
			result[i] = ""
		}
	}
	return result
}

func buildLine(pType string, vals ...string) string {
	var sb strings.Builder
	sb.WriteString(pType)
	lastNonEmpty := -1
	for i := len(vals) - 1; i >= 0; i-- {
		if vals[i] != "" {
			lastNonEmpty = i
			break
		}
	}
	for i := 0; i <= lastNonEmpty; i++ {
		sb.WriteString(", ")
		sb.WriteString(vals[i])
	}
	return sb.String()
}
