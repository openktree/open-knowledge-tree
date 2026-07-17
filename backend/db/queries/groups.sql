-- name: CreateGroup :one
INSERT INTO groups (name, description) VALUES ($1, $2) RETURNING *;

-- name: GetGroupByID :one
SELECT * FROM groups WHERE id = $1;

-- name: GetGroupByName :one
SELECT * FROM groups WHERE name = $1;

-- name: ListGroups :many
SELECT * FROM groups ORDER BY name ASC;

-- name: UpdateGroup :one
UPDATE groups SET name = $2, description = $3, updated_at = now()
WHERE id = $1 RETURNING *;

-- name: DeleteGroup :exec
DELETE FROM groups WHERE id = $1;

-- name: AddGroupMember :exec
INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)
ON CONFLICT (group_id, user_id) DO NOTHING;

-- name: RemoveGroupMember :exec
DELETE FROM group_members WHERE group_id = $1 AND user_id = $2;

-- name: ListGroupMembers :many
SELECT gm.group_id, gm.user_id, gm.joined_at,
       u.email, u.display_name
FROM group_members gm
JOIN users u ON u.id = gm.user_id
WHERE gm.group_id = $1
ORDER BY gm.joined_at ASC;

-- name: ListGroupsForUser :many
SELECT g.* FROM groups g
JOIN group_members gm ON gm.group_id = g.id
WHERE gm.user_id = $1
ORDER BY g.name ASC;

-- name: IsGroupMember :one
SELECT EXISTS (
    SELECT 1 FROM group_members
    WHERE group_id = $1 AND user_id = $2
) AS is_member;
