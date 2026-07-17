-- name: ListUsers :many
SELECT * FROM users ORDER BY created_at DESC;

-- name: ListUsersWithRoles :many
SELECT u.* FROM users u ORDER BY u.created_at DESC;
