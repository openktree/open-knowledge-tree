package auth

import "context"

type ck string

const (
	ckUserID   ck = "user_id"
	ckUserRole  ck = "user_role"
	ckUserEmail ck = "user_email"
)

func WithUser(ctx context.Context, userID, role string) context.Context {
	ctx = context.WithValue(ctx, ckUserID, userID)
	ctx = context.WithValue(ctx, ckUserRole, role)
	return ctx
}

func RequestUser(ctx context.Context) string {
	v, _ := ctx.Value(ckUserID).(string)
	return v
}

func RequestUserRole(ctx context.Context) string {
	v, _ := ctx.Value(ckUserRole).(string)
	return v
}

func WithUserEmail(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, ckUserEmail, email)
}

func RequestUserEmail(ctx context.Context) string {
	v, _ := ctx.Value(ckUserEmail).(string)
	return v
}
