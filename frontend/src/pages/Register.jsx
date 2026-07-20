import { useNavigate } from "@solidjs/router";
import { createSignal, onMount } from "solid-js";
import Alert from "../components/Alert";
import Button from "../components/Button";
import Card from "../components/Card";
import FormField from "../components/FormField";
import { api } from "../services/api";

export default function Register() {
  const [displayName, setDisplayName] = createSignal("");
  const [email, setEmail] = createSignal("");
  const [password, setPassword] = createSignal("");
  const [error, setError] = createSignal("");
  const [loading, setLoading] = createSignal(false);
  const navigate = useNavigate();

  onMount(() => {
    if (localStorage.getItem("token")) {
      navigate("/dashboard", { replace: true });
    }
  });

  const handleSubmit = async (e) => {
    e.preventDefault();
    setError("");
    setLoading(true);

    try {
      await api.register({
        email: email(),
        password: password(),
        display_name: displayName(),
      });

      navigate("/login", { replace: true });
    } catch (err) {
      setError(err.message);
    } finally {
      setLoading(false);
    }
  };

  return (
    <div class="min-h-screen flex items-center justify-center bg-page transition-colors">
      <form onSubmit={handleSubmit} class="w-full max-w-sm">
        <Card>
          <h1 class="text-2xl font-bold mb-6 text-center text-text-base">Create Account</h1>

          <Alert variant="error" message={error()} onDismiss={() => setError("")} />

          <FormField
            label="Display Name"
            value={displayName()}
            onChange={setDisplayName}
            required
            class="mb-4"
          />
          <FormField
            label="Email"
            type="email"
            value={email()}
            onChange={setEmail}
            required
            class="mb-4"
          />
          <FormField
            label="Password"
            type="password"
            value={password()}
            onChange={setPassword}
            required
            class="mb-6"
          />

          <Button type="submit" loading={loading()} class="w-full">
            Create Account
          </Button>

          <p class="mt-4 text-center text-sm text-text-muted">
            Already have an account?{" "}
            <a href="/login" class="text-link hover:underline">
              Sign In
            </a>
          </p>
        </Card>
      </form>
    </div>
  );
}
