import { defineRailway, postgres, project, service } from "railway/iac";

// Local source upload is selected separately; this file does not publish a repository.
export default defineRailway(() => {
  const db = postgres("postgres");
  const api = service("multi-user-ai-context-manager", {
    start: "/agent-context serve",
    preDeploy: "/agent-context migrate",
    healthcheck: "/readyz",
    healthcheckTimeout: 60,
    replicas: 1,
    env: { DATABASE_URL: db.env.DATABASE_URL },
  });
  return project("multi-user-ai-context-manager", { resources: [db, api] });
});
