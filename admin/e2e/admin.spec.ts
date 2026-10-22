import { expect, test, type Page } from "@playwright/test";

const email = process.env.ADMIN_EMAIL ?? "";
const password = process.env.ADMIN_PASSWORD ?? "";

test.beforeAll(() => {
  if (!process.env.BASE_URL || !email || !password) {
    throw new Error("set BASE_URL, ADMIN_EMAIL and ADMIN_PASSWORD (see playwright.config.ts)");
  }
});

async function signIn(page: Page) {
  await page.goto("./");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill(password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByTestId("signed-in-as")).toHaveText(email);
}

test("rejects a wrong password", async ({ page }) => {
  await page.goto("./");
  await expect(page).toHaveTitle("LCARS Admin");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password").fill("not-the-password");
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("alert")).toHaveText("invalid email or password");
  await expect(page.getByTestId("signed-in-as")).toHaveCount(0);
});

test("creates, uses and revokes a device token", async ({ page, request }) => {
  await signIn(page);
  const name = `Playwright display ${Date.now()}`;

  // Create: the credentials are shown once.
  await page.getByLabel("Device name").fill(name);
  await page.getByRole("button", { name: "Create token" }).click();
  const card = page.getByTestId("new-token");
  await expect(card).toContainText(name);
  const username = await page.getByTestId("new-token-username").inputValue();
  const secret = await page.getByTestId("new-token-password").inputValue();
  expect(username).toMatch(/^dev_[a-z0-9]{12}$/);
  expect(secret).toMatch(/^[A-Za-z0-9_-]{43}$/);
  const basic = Buffer.from(`${username}:${secret}`).toString("base64");
  await expect(page.getByTestId("new-token-header")).toHaveValue(`Authorization: Basic ${basic}`);
  await expect(page.getByTestId("new-token-nats-url")).toHaveValue(
    new RegExp(`^nats://${username}:${secret}@[^/]+:4222$`),
  );
  await expect(page.getByLabel("Device name")).toHaveValue("");

  // It is listed, without its secret, and works on a device endpoint.
  const row = page.getByTestId(`token-${username}`);
  await expect(row).toContainText(name);
  await expect(row).not.toContainText(secret);
  const calendar = (h?: Record<string, string>) => request.get("../calendar?from=2030-03-04&days=1", { headers: h });
  expect((await calendar({ Authorization: `Basic ${basic}` })).status()).toBe(200);
  expect((await calendar()).status()).toBe(401);

  // The session survives a reload; the secret does not come back.
  await page.reload();
  await expect(page.getByTestId("signed-in-as")).toHaveText(email);
  await expect(page.getByTestId("new-token")).toHaveCount(0);
  await expect(row).toBeVisible();

  // Revoke, after confirming.
  page.once("dialog", (d) => d.accept());
  await row.getByRole("button", { name: `Revoke ${name}` }).click();
  await expect(row).toHaveCount(0);
  expect((await calendar({ Authorization: `Basic ${basic}` })).status()).toBe(401);
});

test("dismissing the revoke dialog keeps the token", async ({ page }) => {
  await signIn(page);
  const name = `Kept display ${Date.now()}`;
  await page.getByLabel("Device name").fill(name);
  await page.getByRole("button", { name: "Create token" }).click();
  const username = await page.getByTestId("new-token-username").inputValue();
  await page.getByRole("button", { name: "Done" }).click();
  await expect(page.getByTestId("new-token")).toHaveCount(0);

  const row = page.getByTestId(`token-${username}`);
  page.once("dialog", (d) => d.dismiss());
  await row.getByRole("button", { name: `Revoke ${name}` }).click();
  await page.reload();
  await expect(row).toBeVisible();

  // Clean up.
  page.once("dialog", (d) => d.accept());
  await row.getByRole("button", { name: `Revoke ${name}` }).click();
  await expect(row).toHaveCount(0);
});

test("signs out, and back in after the session is lost", async ({ page }) => {
  await signIn(page);
  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByLabel("Email")).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("Email")).toBeVisible();

  // A session the server rejects (e.g. expired) sends the admin to sign in.
  await signIn(page);
  await page.evaluate(() => {
    const s = JSON.parse(sessionStorage.getItem("lcars-admin-session") ?? "{}");
    sessionStorage.setItem("lcars-admin-session", JSON.stringify({ ...s, access_token: "expired" }));
  });
  await page.reload();
  await expect(page.getByRole("status")).toHaveText("Your session has expired. Sign in again.");
  await expect(page.getByLabel("Email")).toBeVisible();
});
