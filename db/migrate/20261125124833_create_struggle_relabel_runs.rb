class CreateStruggleRelabelRuns < ActiveRecord::Migration[8.1]
  def change
    # One request to re-check a submission's labels with an external model.
    # reserved_cents holds a conservative cost estimate from before the call,
    # so concurrent runs cannot overshoot the monthly cap; cost_cents is the
    # actual cost once the provider answers. Proposals never change hours.
    create_table :struggle_relabel_runs do |t|
      t.references :struggle_submission, null: false, foreign_key: true
      t.references :requested_by, null: false, foreign_key: { to_table: :users }
      t.text :reason, null: false
      t.string :status, null: false, default: "queued"
      t.string :provider, null: false
      t.string :model, null: false
      t.string :prompt_version, null: false
      t.string :bundle_digest, null: false
      t.integer :payload_bytes, null: false
      t.integer :reserved_cents, null: false
      t.integer :cost_cents
      t.jsonb :proposals
      t.text :error
      t.datetime :finished_at
      t.timestamps
    end
    add_index :struggle_relabel_runs, :created_at
  end
end
