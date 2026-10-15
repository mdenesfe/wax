-- Separate database for integration tests (WAX_TEST_DATABASE_URL), so tests
-- never touch development data.
CREATE DATABASE wax_test OWNER wax;
