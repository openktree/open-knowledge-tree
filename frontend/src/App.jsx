import { Route, useNavigate } from "@solidjs/router";
import AuthGuard from "./components/AuthGuard";
import AIUsage from "./pages/AIUsage";
import ConceptDetail from "./pages/ConceptDetail";
import Concepts from "./pages/Concepts";
import Dashboard from "./pages/Dashboard";
import FactDetail from "./pages/FactDetail";
import Facts from "./pages/Facts";
import Investigation from "./pages/Investigation";
import Investigations from "./pages/Investigations";
import Login from "./pages/Login";
import Promptsets from "./pages/Promptsets";
import Providers from "./pages/Providers";
import Register from "./pages/Register";
import Remote from "./pages/Remote";
import ReportDetail from "./pages/ReportDetail";
import Reports from "./pages/Reports";
import Repositories from "./pages/Repositories";
import RepositorySettings from "./pages/RepositorySettings";
import SourceDetail from "./pages/SourceDetail";
import Sources from "./pages/Sources";
import Tasks from "./pages/Tasks";
import Users from "./pages/Users";
import { setUnauthorizedCallback } from "./services/api";
import { setToken } from "./store/auth";
import { RBACProvider } from "./store/rbac";
import { RepositoryProvider } from "./store/repository";

function AppLayout(props) {
  const navigate = useNavigate();

  setUnauthorizedCallback(() => {
    localStorage.removeItem("token");
    setToken(null);
    navigate("/login", { replace: true });
  });

  return (
    <RBACProvider>
      <RepositoryProvider>{props.children}</RepositoryProvider>
    </RBACProvider>
  );
}

export default function App() {
  return (
    <Route component={AppLayout}>
      <Route component={AuthGuard}>
        <Route path="/" component={Dashboard} />
        <Route path="/dashboard" component={Dashboard} />
        <Route path="/sources" component={Sources} />
        <Route path="/facts" component={Facts} />
        <Route path="/concepts" component={Concepts} />
        <Route path="/providers" component={Providers} />
        <Route path="/:slug/sources/:sourceID" component={SourceDetail} />
        <Route path="/:slug/facts/:factID" component={FactDetail} />
        <Route path="/:slug/concepts/:conceptID" component={ConceptDetail} />
        <Route path="/repositories" component={Repositories} />
        <Route path="/repositories/:repoID/settings" component={RepositorySettings} />
        <Route path="/investigations" component={Investigations} />
        <Route path="/:slug/investigations/:invID" component={Investigation} />
        <Route path="/:slug/investigations/:invID/:phase" component={Investigation} />
        <Route path="/reports" component={Reports} />
        <Route path="/:slug/reports/:reportID" component={ReportDetail} />
        <Route path="/tasks" component={Tasks} />
        <Route path="/users" component={Users} />
        <Route path="/remote" component={Remote} />
        <Route path="/promptsets" component={Promptsets} />
        <Route path="/ai-usage" component={AIUsage} />
      </Route>
      <Route path="/login" component={Login} />
      <Route path="/register" component={Register} />
    </Route>
  );
}
