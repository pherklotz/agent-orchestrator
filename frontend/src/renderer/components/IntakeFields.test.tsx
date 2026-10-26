import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { buildIntake, IntakeFields, intakeNeedsJiraProject, type IntakeForm } from "./IntakeFields";

const jira: IntakeForm = { enabled: true, provider: "jira", repo: "", assignee: "me@example.com" };

describe("tracker intake provider", () => {
	it("requires a Jira project key and serializes the provider", () => {
		expect(intakeNeedsJiraProject(jira)).toBe(true);
		expect(intakeNeedsJiraProject({ ...jira, repo: "PROJ" })).toBe(false);
		expect(intakeNeedsJiraProject({ ...jira, provider: "" })).toBe(false);
		expect(buildIntake({ ...jira, repo: "PROJ" })).toEqual({ enabled: true, provider: "jira", repo: "PROJ", assignee: "me@example.com" });
		// "" means auto-detect and clears a stored provider; undefined keeps it.
		expect(buildIntake({ ...jira, provider: "", repo: "" }, { provider: "gitlab" })?.provider).toBeUndefined();
		expect(buildIntake({ enabled: true, repo: "", assignee: "a" }, { provider: "gitlab" })?.provider).toBe("gitlab");
	});

	it("picks Jira from the tracker menu and edits the project key", async () => {
		const onChange = vi.fn();
		const { rerender } = render(<IntakeFields variant="settings" form={{ ...jira, provider: "" }} onChange={onChange} repoPreview={{ value: "acme/app" }} />);
		expect(screen.getByText("acme/app")).toBeInTheDocument();
		await userEvent.click(screen.getByRole("button", { name: "Tracker" }));
		await userEvent.click(await screen.findByRole("menuitem", { name: "Jira" }));
		expect(onChange).toHaveBeenLastCalledWith({ provider: "jira", repo: "" });

		rerender(<IntakeFields variant="settings" form={jira} onChange={onChange} repoPreview={{ value: "acme/app" }} />);
		expect(screen.queryByText("acme/app")).not.toBeInTheDocument();
		expect(screen.getByText(/Enter the Jira project key/)).toBeInTheDocument();
		await userEvent.click(screen.getByRole("button", { name: "Edit Jira project" }));
		await userEvent.type(screen.getByRole("textbox", { name: "Jira project" }), "p");
		expect(onChange).toHaveBeenLastCalledWith({ repo: "P" });
	});
});
