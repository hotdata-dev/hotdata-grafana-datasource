import { test, expect } from '@grafana/plugin-e2e';

// The SQL editor is Monaco; its textarea carries Monaco's own accessible name.
const sqlEditor = /Editor content/;

test('smoke: should render query editor', async ({ panelEditPage, readProvisionedDataSource }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await panelEditPage.datasource.set(ds.name);
  await expect(panelEditPage.getQueryEditorRow('A').getByRole('textbox', { name: sqlEditor })).toBeVisible();
});

// Requires the mock API (`go run ./pkg/cmd/mockapi`) or a live workspace
// behind the provisioned datasource.
test('table query should return the mock series', async ({ panelEditPage, readProvisionedDataSource, page }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await panelEditPage.datasource.set(ds.name);
  // Pick the visualization before writing SQL: switching it afterwards
  // re-renders the panel, which cancels the in-flight query kicked off when
  // the editor loses focus and races the refresh below.
  await panelEditPage.setVisualization('Table');
  await panelEditPage.getQueryEditorRow('A').getByRole('textbox', { name: sqlEditor }).click();
  await page.keyboard.type('SELECT ts, service, value FROM metrics');
  // Dismiss Monaco's completion popup so it cannot swallow the refresh click.
  await page.keyboard.press('Escape');
  await expect(panelEditPage.refreshPanel()).toBeOK();
  await expect(panelEditPage.panel.data).toContainText(['api']);
});

// The database picker searches by name on the server (mock API: one database,
// "mock-db"). Combobox options render in a portal listbox, so they are found by
// role on the page.
test('database picker should search databases by name', async ({ panelEditPage, readProvisionedDataSource, page }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await panelEditPage.datasource.set(ds.name);
  const picker = panelEditPage.getQueryEditorRow('A').getByRole('combobox', { name: 'Database' });
  const option = page.getByRole('option', { name: /mock-db/ });
  await picker.click();
  await picker.fill('MOCK');
  await expect(option).toBeVisible();
  await picker.fill('no-such-db');
  await expect(option).toBeHidden();
  await picker.fill('mock');
  await option.click();
  await expect(picker).toHaveValue('mock-db');
});
