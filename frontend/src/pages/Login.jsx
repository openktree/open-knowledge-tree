import { createSignal, onMount } from "solid-js";
import { useNavigate } from "@solidjs/router";
import { api } from "../services/api";
import { setToken } from "../store/auth";
import Alert from "../components/Alert";
import Button from "../components/Button";
import FormField from "../components/FormField";
import Card from "../components/Card";

export default function Login() {
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
      const data = await api.login({
        email: email(),
        password: password(),
      });

      localStorage.setItem("token", data.token);
      setToken(data.token);
      navigate("/dashboard", { replace: true });
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
          <h1 class="text-2xl font-bold mb-6 text-center text-text-base">Sign In</h1>

          <Alert variant="error" message={error()} onDismiss={() => setError("")} />

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
            Sign In
          </Button>

          <p class="mt-4 text-center text-sm text-text-muted">
            Don't have an account?{" "}
            <a href="/register" class="text-link hover:underline">
              Register
            </a>
          </p>
        </Card>
      </form>
    </div>
  );
}
