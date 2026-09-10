-- +goose Up
ALTER TABLE valkey_instances ADD COLUMN user_email TEXT;

-- +goose StatementBegin
CREATE FUNCTION set_valkey_instance_user_email()
RETURNS TRIGGER
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.user_email IS NULL THEN
        SELECT email INTO NEW.user_email
        FROM users
        WHERE id = NEW.user_id;
    END IF;

    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER valkey_instances_set_user_email
BEFORE INSERT ON valkey_instances
FOR EACH ROW
EXECUTE FUNCTION set_valkey_instance_user_email();

UPDATE valkey_instances AS instance
SET user_email = users.email
FROM users
WHERE users.id = instance.user_id;

ALTER TABLE valkey_instances
    ALTER COLUMN user_email SET NOT NULL,
    ADD CONSTRAINT valkey_instances_user_email_normalized
        CHECK (user_email <> '' AND user_email = lower(btrim(user_email)));

-- +goose Down
DROP TRIGGER valkey_instances_set_user_email ON valkey_instances;
DROP FUNCTION set_valkey_instance_user_email();
ALTER TABLE valkey_instances DROP COLUMN user_email;
