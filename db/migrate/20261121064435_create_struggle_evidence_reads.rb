class CreateStruggleEvidenceReads < ActiveRecord::Migration[8.1]
  def change
    # Every time someone other than the submitter opens a submission's
    # evidence, and under which permission.
    create_table :struggle_evidence_reads do |t|
      t.references :struggle_submission, null: false, foreign_key: true
      t.references :user, null: false, foreign_key: true
      t.string :scope, null: false
      t.string :access, null: false
      t.datetime :created_at, null: false
    end
  end
end
