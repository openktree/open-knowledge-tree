package store

func (q *Queries) DB() DBTX {
	return q.db
}
