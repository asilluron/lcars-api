import { defineConfig, devices } from "@playwright/test";

// The browser tests drive the admin page of a running lcars-api. They are run
// by the Go e2e suite (TestAdminUI in e2e/admin_ui_test.go), which starts the
// server, Supabase and an admin account, and passes them in:
//
//   BASE_URL        the admin page, e.g. http://lcars:8080/admin/
//   ADMIN_EMAIL     an admin account's email
//   ADMIN_PASSWORD  and its password
export default defineConfig({
  testDir: "./e2e",
  forbidOnly: true,
  retries: 0,
  workers: 1,
  reporter: [["list"]],
  use: {
    baseURL: process.env.BASE_URL,
    trace: "off",
  },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
